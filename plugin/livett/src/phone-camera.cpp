// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

#include "phone-camera.hpp"

#include <obs-module.h>
#include <obs-frontend-api.h>
#include <obs.h>
#include <obs.hpp>
#include <media-io/audio-io.h>

#include <QApplication>
#include <QClipboard>
#include <QCloseEvent>
#include <QHBoxLayout>
#include <QDialog>
#include <QImage>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QLabel>
#include <QNetworkAccessManager>
#include <QNetworkReply>
#include <QNetworkRequest>
#include <QPixmap>
#include <QPointer>
#include <QPushButton>
#include <QTimer>
#include <QUrl>
#include <QVBoxLayout>


#include <functional>
#include <string>

namespace {

constexpr const char *kSourceId = "tiktok_phone_camera";
constexpr const char *kInstallCommand = "winget install Cloudflare.cloudflared";

// Where the plugin's controller listens. TIKTOK_PLUGIN_BASE exists only to point it at a test stub.
QString controller_base()
{
	static const QString value = qEnvironmentVariableIsSet("TIKTOK_PLUGIN_BASE") ? qEnvironmentVariable("TIKTOK_PLUGIN_BASE")
										       : QStringLiteral("http://127.0.0.1:8765");
	return value;
}

QNetworkAccessManager &network()
{
	static QNetworkAccessManager *manager = new QNetworkAccessManager();
	return *manager;
}

using Done = std::function<void(int status, const QByteArray &body)>;

// One request to the controller; `done` runs on the UI thread (status 0 = no answer). `context` ends the callback if it dies first.
void send(const QByteArray &method, const QString &path, const QByteArray &body, const QString &token, QObject *context, Done done,
	  int timeoutMs = 15000)
{
	QNetworkRequest request{QUrl(controller_base() + path)};
	if (!token.isEmpty()) {
		request.setRawHeader("Origin", controller_base().toUtf8());
		request.setRawHeader("X-Panel-Token", token.toUtf8());
	}
	request.setHeader(QNetworkRequest::ContentTypeHeader, QStringLiteral("application/json"));
	QNetworkReply *reply = method == "POST" ? network().post(request, body) : network().get(request);
	auto *timer = new QTimer(reply);
	timer->setSingleShot(true);
	QObject::connect(timer, &QTimer::timeout, reply, &QNetworkReply::abort);
	timer->start(timeoutMs);
	QObject::connect(reply, &QNetworkReply::finished, context, [reply, done] {
		const int status = reply->attribute(QNetworkRequest::HttpStatusCodeAttribute).toInt();
		const QByteArray data = reply->readAll();
		reply->deleteLater();
		done(status, data);
	});
}

// A request that needs the panel's token (the controller's mutating calls).
void send_with_token(const QString &path, const QJsonObject &payload, QObject *context, Done done, int timeoutMs = 15000)
{
	send("GET", QStringLiteral("/api/session"), {}, {}, context, [=](int status, const QByteArray &body) {
		const QString token = QJsonDocument::fromJson(body).object().value(QStringLiteral("token")).toString();
		if (status != 200 || token.isEmpty()) {
			done(0, {});
			return;
		}
		send("POST", path, QJsonDocument(payload).toJson(QJsonDocument::Compact), token, context, done, timeoutMs);
	}, 5000);
}

QJsonObject object_of(const QByteArray &body) { return QJsonDocument::fromJson(body).object(); }

QString error_of(const QByteArray &body, const QString &fallback)
{
	const QString text = object_of(body).value(QStringLiteral("error")).toString();
	return text.isEmpty() ? fallback : text;
}

// ---- the pairing window

class PairingDialog : public QDialog {
public:
	static void show_one(const QString &id)
	{
		static QPointer<PairingDialog> current;
		if (current && current->id_ == id) {
			current->show();
			current->raise();
			current->activateWindow();
			return;
		}
		if (current)
			current->close(); // another camera's window: it closes first (and stops that pairing if no phone came)
		current = new PairingDialog(id, static_cast<QWidget *>(obs_frontend_get_main_window()));
		current->setAttribute(Qt::WA_DeleteOnClose);
		current->show();
		current->raise();
		current->activateWindow();
		current->begin();
	}

private:
	explicit PairingDialog(const QString &id, QWidget *parent) : QDialog(parent), id_(id)
	{
		setWindowTitle(QStringLiteral("Parear o celular"));
		setMinimumWidth(380);
		auto *layout = new QVBoxLayout(this);
		status_ = new QLabel();
		status_->setWordWrap(true);
		qr_ = new QLabel();
		qr_->setAlignment(Qt::AlignCenter);
		qr_->setMinimumHeight(250);
		note_ = new QLabel();
		note_->setWordWrap(true);
		note_->setTextInteractionFlags(Qt::TextSelectableByMouse);
		copy_ = new QPushButton(QStringLiteral("Copiar comando de instalação"));
		copy_->hide();
		auto *renew = new QPushButton(QStringLiteral("Novo código"));
		auto *close = new QPushButton(QStringLiteral("Fechar"));
		auto *buttons = new QHBoxLayout();
		buttons->addWidget(renew);
		buttons->addStretch(1);
		buttons->addWidget(close);
		layout->addWidget(status_);
		layout->addWidget(qr_);
		layout->addWidget(note_);
		layout->addWidget(copy_);
		layout->addLayout(buttons);

		QObject::connect(close, &QPushButton::clicked, this, &QDialog::close);
		QObject::connect(renew, &QPushButton::clicked, this, [this] { renew_code(); });
		QObject::connect(copy_, &QPushButton::clicked, this, [] { QApplication::clipboard()->setText(QString::fromUtf8(kInstallCommand)); });
		poll_ = new QTimer(this);
		QObject::connect(poll_, &QTimer::timeout, this, [this] { refresh(); });
	}

	void closeEvent(QCloseEvent *event) override
	{
		poll_->stop();
		// Closed without a phone: nobody is pairing any more, so the door is closed again.
		if (!connected_)
			send_with_token(QStringLiteral("/api/camera/stop"), QJsonObject{{"id", id_}}, qApp, [](int, const QByteArray &) {});
		QDialog::closeEvent(event);
	}

	void begin()
	{
		status_->setText(QStringLiteral("Abrindo a conexão segura… (pode levar alguns segundos)"));
		send_with_token(QStringLiteral("/api/camera/pair"), QJsonObject{{"id", id_}}, this, [this](int status, const QByteArray &body) {
			if (status != 200) {
				status_->setText(status == 0 ? QStringLiteral("O controlador do LiveTT não está respondendo. Abra o painel \"LiveTT\" e tente de novo.")
							     : error_of(body, QStringLiteral("Não consegui abrir a conexão.")));
				return;
			}
			load_qr();
			poll_->start(1500);
			refresh();
		}, 70000);
	}

	void renew_code()
	{
		status_->setText(QStringLiteral("Gerando um novo código…"));
		send_with_token(QStringLiteral("/api/camera/revoke"), QJsonObject{{"id", id_}}, this, [this](int status, const QByteArray &body) {
			if (status != 200) {
				status_->setText(error_of(body, QStringLiteral("Não consegui gerar um novo código.")));
				return;
			}
			begin();
		});
	}

	void load_qr()
	{
		send("GET", QStringLiteral("/camera/qr.txt?i=0&c=") + QString::fromUtf8(QUrl::toPercentEncoding(id_)), {}, {}, this, [this](int status, const QByteArray &body) {
			const QJsonArray rows = object_of(body).value(QStringLiteral("rows")).toArray();
			if (status != 200 || rows.isEmpty()) {
				qr_->setText(QStringLiteral("(sem código)"));
				return;
			}
			const int n = rows.size(), quiet = 4;
			QImage image(n + 2 * quiet, n + 2 * quiet, QImage::Format_RGB32);
			image.fill(Qt::white);
			for (int y = 0; y < n; y++) {
				const QString row = rows.at(y).toString();
				for (int x = 0; x < n && x < row.size(); x++)
					if (row.at(x) == QLatin1Char('1'))
						image.setPixel(x + quiet, y + quiet, qRgb(0, 0, 0));
			}
			const int scale = qMax(1, 240 / image.width());
			qr_->setPixmap(QPixmap::fromImage(image.scaled(image.size() * scale, Qt::KeepAspectRatio, Qt::FastTransformation)));
		});
	}

	void refresh()
	{
		send("GET", QStringLiteral("/api/state"), {}, {}, this, [this](int status, const QByteArray &body) {
			if (status != 200)
				return;
			const QJsonObject state = object_of(body).value(QStringLiteral("camera")).toObject();
			QJsonObject camera = state; // this camera's own row (connected, name, phone) over the shared state (mode, hints)
			const QJsonArray rows = state.value(QStringLiteral("cameras")).toArray();
			for (const QJsonValue &value : rows) {
				const QJsonObject mine = value.toObject(); // one object, kept: begin() and end() must come from the same one
				if (mine.value(QStringLiteral("id")).toString() != id_)
					continue;
				for (auto it = mine.begin(); it != mine.end(); ++it)
					camera.insert(it.key(), it.value());
				setWindowTitle(QStringLiteral("Parear o celular: ") + mine.value(QStringLiteral("name")).toString());
			}
			const QString mode = camera.value(QStringLiteral("mode")).toString();
			const bool relay = mode == QLatin1String("relay");
			const bool tunnel = mode == QLatin1String("tunnel") || relay; // both give the phone a page with a valid certificate
			if (camera.value(QStringLiteral("connected")).toBool()) {
				connected_ = true;
				const QString label = camera.value(QStringLiteral("label")).toString();
				status_->setText(QStringLiteral("Celular conectado") + (label.isEmpty() ? QString() : QStringLiteral(": ") + label) +
						 QStringLiteral(". Pode fechar esta janela."));
				poll_->stop();
				QTimer::singleShot(2500, this, &QDialog::close);
				return;
			}
			status_->setText(tunnel ? QStringLiteral("Aponte a câmera do celular para o código e toque em \"Iniciar câmera\" na página que abrir.")
						: QStringLiteral("Aponte a câmera do celular para o código. Se o navegador avisar sobre a conexão, toque em \"Avançado\" e \"Continuar\" "
								 "(o certificado é do seu computador) e depois em \"Iniciar câmera\"."));
			const bool needs = camera.value(QStringLiteral("needsTunnel")).toBool();
			copy_->setVisible(needs);
			note_->setText(needs ? QStringLiteral("Para o iPhone (e sem aviso de certificado) instale uma vez o cloudflared: abra o PowerShell, rode\n%1\ne toque em \"Novo código\". "
							      "Sem ele a câmera usa a rede local e o iPhone costuma recusá-la.").arg(QString::fromUtf8(kInstallCommand))
					     : (relay ? QStringLiteral("Conexão segura pelo servidor da câmera do projeto: ele só leva as mensagens para ligar; o vídeo e o som vão direto do celular para o OBS.")
						       : tunnel ? QStringLiteral("Conexão segura pela Cloudflare: só a página e as mensagens para ligar passam por ela; o vídeo vai direto do celular para o OBS.")
						       : QString()));
		});
	}

	QString id_; // the camera this window pairs
	QLabel *status_ = nullptr, *qr_ = nullptr, *note_ = nullptr;
	QPushButton *copy_ = nullptr;
	QTimer *poll_ = nullptr;
	bool connected_ = false;
};

// ---- the source

struct PhoneCamera {
	obs_source_t *self = nullptr;
	obs_source_t *child = nullptr; // a private Browser Source that loads the receiver page
	uint32_t width = 1920, height = 1080;
	bool heard = false;      // the first sound from the phone was passed on
	std::string camera_id;   // the camera (pairing) this source shows; kept in the source's settings
};

void audio_from_child(void *param, obs_source_t *, const struct audio_data *audio, bool muted)
{
	auto *camera = static_cast<PhoneCamera *>(param);
	if (muted || !audio || audio->frames == 0)
		return;
	obs_audio_info info;
	if (!obs_get_audio_info(&info))
		return;
	obs_source_audio out = {};
	for (int i = 0; i < MAX_AV_PLANES; i++)
		out.data[i] = audio->data[i];
	out.frames = audio->frames;
	out.speakers = info.speakers;
	out.format = AUDIO_FORMAT_FLOAT_PLANAR;
	out.samples_per_sec = info.samples_per_sec;
	out.timestamp = audio->timestamp;
	if (!camera->heard) {
		camera->heard = true;
		blog(LOG_INFO, "[livett] phone camera: sound from the phone is reaching OBS (%u Hz)", info.samples_per_sec);
	}
	obs_source_output_audio(camera->self, &out);
}

// The source takes the shape of the picture that arrives (upright or on its side): the controller reports its size.
void apply_size(PhoneCamera *camera, uint32_t w, uint32_t h)
{
	if (w < 16 || h < 16 || w > 4096 || h > 4096 || (w == camera->width && h == camera->height))
		return;
	blog(LOG_INFO, "[livett] phone camera: the picture is now %ux%u", w, h);
	camera->width = w;
	camera->height = h;
	if (camera->child) {
		OBSDataAutoRelease update = obs_data_create();
		obs_data_set_int(update, "width", w);
		obs_data_set_int(update, "height", h);
		obs_source_update(camera->child, update);
	}
}

// Asks the controller for the picture's size every moment while the source exists (a small loopback request).
void poll_size(OBSWeakSource weak)
{
	OBSSourceAutoRelease source = obs_weak_source_get_source(weak);
	if (!source)
		return; // the source is gone: the polling ends with it
	auto *current = static_cast<PhoneCamera *>(obs_obj_get_data(source));
	if (!current || current->camera_id.empty()) { // its camera is not known yet (the controller may still be starting)
		QTimer::singleShot(1500, qApp, [weak] { poll_size(weak); });
		return;
	}
	const QString path = QStringLiteral("/camera/size?c=") + QString::fromUtf8(QUrl::toPercentEncoding(QString::fromStdString(current->camera_id)));
	send("GET", path, {}, {}, qApp, [weak](int status, const QByteArray &body) {
		OBSSourceAutoRelease source = obs_weak_source_get_source(weak);
		if (!source)
			return;
		if (status == 200) {
			const QJsonObject size = object_of(body);
			const int w = size.value(QStringLiteral("width")).toInt(), h = size.value(QStringLiteral("height")).toInt();
			auto *camera = static_cast<PhoneCamera *>(obs_obj_get_data(source));
			if (camera && w > 0 && h > 0)
				apply_size(camera, uint32_t(w), uint32_t(h));
		}
		QTimer::singleShot(1500, qApp, [weak] { poll_size(weak); });
	}, 4000);
}


// Asks the controller for this source's camera (the one it remembers, else a free or new one), points the child at that camera's
// receiver and remembers it in the settings; the first time the source is added, shows the QR.
void resolve_receiver(OBSWeakSource weak, bool open_pairing, int attempt)
{
	OBSSourceAutoRelease source = obs_weak_source_get_source(weak);
	if (!source)
		return; // the source is gone
	OBSDataAutoRelease saved = obs_source_get_settings(source);
	const QString remembered = QString::fromUtf8(obs_data_get_string(saved, "camera_id"));
	send_with_token(QStringLiteral("/api/camera/claim"), QJsonObject{{"id", remembered}}, qApp, [weak, open_pairing, attempt](int status, const QByteArray &body) {
		OBSSourceAutoRelease source = obs_weak_source_get_source(weak);
		if (!source)
			return;
		const QString id = object_of(body).value(QStringLiteral("id")).toString();
		const QString url = object_of(body).value(QStringLiteral("sourceURL")).toString();
		if (status != 200 || url.isEmpty() || id.isEmpty()) {
			// the controller may still be starting (OBS has just opened): try again a few times
			if (attempt < 30)
				QTimer::singleShot(2000, qApp, [weak, open_pairing, attempt] { resolve_receiver(weak, open_pairing, attempt + 1); });
			return;
		}
		auto *camera = static_cast<PhoneCamera *>(obs_obj_get_data(source));
		if (camera) {
			camera->camera_id = id.toStdString();
			if (camera->child) {
				OBSDataAutoRelease update = obs_data_create();
				obs_data_set_string(update, "url", url.toUtf8().constData());
				obs_source_update(camera->child, update);
			}
		}
		OBSDataAutoRelease settings = obs_source_get_settings(source);
		obs_data_set_string(settings, "camera_id", id.toUtf8().constData()); // saved with the scene collection
		if (open_pairing) {
			obs_data_set_bool(settings, "pairing_shown", true);
			PairingDialog::show_one(id);
		}
	});
}

const char *pc_get_name(void *) { return "Câmera do celular (LiveTT)"; }

void *pc_create(obs_data_t *settings, obs_source_t *source)
{
	auto *camera = new PhoneCamera();
	camera->self = source;
	camera->camera_id = obs_data_get_string(settings, "camera_id");
	OBSDataAutoRelease child_settings = obs_data_create();
	obs_data_set_bool(child_settings, "is_local_file", false);
	obs_data_set_string(child_settings, "url", "about:blank");
	obs_data_set_bool(child_settings, "reroute_audio", true);
	obs_data_set_bool(child_settings, "fps_custom", true);
	obs_data_set_int(child_settings, "fps", 30);
	obs_data_set_bool(child_settings, "shutdown", false);
	obs_data_set_bool(child_settings, "restart_when_active", false);
	obs_data_set_string(child_settings, "css", ""); // the page sets its own transparent background; its policy refuses injected styles

	camera->child = obs_source_create_private("browser_source", "Receptor da câmera do celular", child_settings);
	if (camera->child) {
		obs_source_set_audio_mixers(camera->child, 0); // the sound reaches OBS through this source, once
		obs_source_add_audio_capture_callback(camera->child, audio_from_child, camera);
	} else {
		blog(LOG_WARNING, "[livett] phone camera: this OBS has no Browser Source");
	}
	OBSWeakSource sizeWeak = OBSGetWeakRef(source);
	QMetaObject::invokeMethod(qApp, [sizeWeak] { poll_size(sizeWeak); }, Qt::QueuedConnection);
	const bool first_time = !obs_data_get_bool(settings, "pairing_shown");
	OBSWeakSource weak = OBSGetWeakRef(source);
	QMetaObject::invokeMethod(qApp, [weak, first_time] { resolve_receiver(weak, first_time, 0); }, Qt::QueuedConnection);
	return camera;
}

void pc_destroy(void *data)
{
	auto *camera = static_cast<PhoneCamera *>(data);
	if (camera->child) {
		obs_source_remove_audio_capture_callback(camera->child, audio_from_child, camera);
		obs_source_release(camera->child);
	}
	delete camera;
}

void pc_update(void *, obs_data_t *) {}

void pc_defaults(obs_data_t *settings)
{
	obs_data_set_default_bool(settings, "pairing_shown", false);
}

bool pair_clicked(obs_properties_t *, obs_property_t *, void *data)
{
	auto *camera = static_cast<PhoneCamera *>(data);
	if (camera && !camera->camera_id.empty())
		PairingDialog::show_one(QString::fromStdString(camera->camera_id));
	return false;
}

obs_properties_t *pc_properties(void *data)
{
	obs_properties_t *props = obs_properties_create();
	obs_properties_add_text(props, "info", "O celular vira a câmera desta fonte: leia o QR code com ele (sem instalar nada) e toque em \"Iniciar câmera\".",
				OBS_TEXT_INFO);
	obs_properties_add_button2(props, "pair", "Parear celular (mostrar o QR code)", pair_clicked, data);
	return props;
}

void pc_render(void *data, gs_effect_t *)
{
	auto *camera = static_cast<PhoneCamera *>(data);
	if (camera->child)
		obs_source_video_render(camera->child);
}

uint32_t pc_width(void *data) { return static_cast<PhoneCamera *>(data)->width; }
uint32_t pc_height(void *data) { return static_cast<PhoneCamera *>(data)->height; }

// A private child is drawn and plays only while its parent says it is showing and active.
void pc_show(void *data) { if (auto *c = static_cast<PhoneCamera *>(data); c->child) obs_source_inc_showing(c->child); }
void pc_hide(void *data) { if (auto *c = static_cast<PhoneCamera *>(data); c->child) obs_source_dec_showing(c->child); }
void pc_activate(void *data) { if (auto *c = static_cast<PhoneCamera *>(data); c->child) obs_source_inc_active(c->child); }
void pc_deactivate(void *data) { if (auto *c = static_cast<PhoneCamera *>(data); c->child) obs_source_dec_active(c->child); }

} // namespace

void tiktok_phone_camera_register()
{
	obs_source_info info = {};
	info.id = kSourceId;
	info.type = OBS_SOURCE_TYPE_INPUT;
	// Custom draw: the Browser Source inside draws itself with its own effect, which must not be nested in the default one.
	info.output_flags = OBS_SOURCE_VIDEO | OBS_SOURCE_AUDIO | OBS_SOURCE_CUSTOM_DRAW | OBS_SOURCE_SRGB | OBS_SOURCE_DO_NOT_DUPLICATE; // one phone, one receiver
	info.icon_type = OBS_ICON_TYPE_CAMERA;
	info.get_name = pc_get_name;
	info.create = pc_create;
	info.destroy = pc_destroy;
	info.update = pc_update;
	info.get_defaults = pc_defaults;
	info.get_properties = pc_properties;
	info.video_render = pc_render;
	info.get_width = pc_width;
	info.get_height = pc_height;
	info.show = pc_show;
	info.hide = pc_hide;
	info.activate = pc_activate;
	info.deactivate = pc_deactivate;
	obs_register_source(&info);
}
