// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

#include "obs-service.hpp"

#include <obs-module.h>
#include <obs-frontend-api.h>
#include <obs.h>
#include <obs.hpp>
#include <util/platform.h>

#include <QApplication>
#include <QComboBox>
#include <QDateTime>
#include <QDialog>
#include <QDir>
#include <QEventLoop>
#include <QFile>
#include <QHBoxLayout>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QKeyEvent>
#include <QLabel>
#include <QLineEdit>
#include <QMainWindow>
#include <QMessageBox>
#include <QMouseEvent>
#include <QNetworkAccessManager>
#include <QNetworkReply>
#include <QNetworkRequest>
#include <QPainter>
#include <QPainterPath>
#include <QPixmap>
#include <QPointer>
#include <QProgressBar>
#include <QPushButton>
#include <QRegularExpression>
#include <QSpinBox>
#include <QStyle>
#include <QThread>
#include <QTimer>
#include <QUrl>
#include <QVBoxLayout>

#include <mutex>
#include <string>

namespace {

constexpr const char *kServiceId = "tiktok_live";

// Where the plugin's controller listens. TIKTOK_PLUGIN_BASE exists only to point it at a test stub.
QString controllerBase()
{
	static const QString value = qEnvironmentVariableIsSet("TIKTOK_PLUGIN_BASE") ? qEnvironmentVariable("TIKTOK_PLUGIN_BASE")
										       : QStringLiteral("http://127.0.0.1:8765");
	return value;
}

QWidget *main_window() { return static_cast<QWidget *>(obs_frontend_get_main_window()); }

// ---- the destination OBS reads when it connects (memory only)

std::mutex g_mutex;
std::string g_server, g_key;
bool g_starting = false;

void set_target(const std::string &server, const std::string &key)
{
	std::lock_guard<std::mutex> lock(g_mutex);
	g_server = server;
	g_key = key;
}

bool has_target()
{
	std::lock_guard<std::mutex> lock(g_mutex);
	return !g_server.empty();
}

// ---- a small HTTP helper that waits with a nested event loop (the UI thread must keep serving the plugin's own
// control API while the controller talks to OBS)

struct Http {
	int status = 0;
	QByteArray body;
	QString error;
};

QNetworkAccessManager &network()
{
	static QNetworkAccessManager *manager = new QNetworkAccessManager();
	return *manager;
}

QNetworkRequest make_request(const QString &path, const QString &token)
{
	QNetworkRequest request(QUrl(controllerBase() + path));
	if (!token.isEmpty()) {
		request.setRawHeader("Origin", controllerBase().toUtf8());
		request.setRawHeader("X-Panel-Token", token.toUtf8());
	}
	request.setHeader(QNetworkRequest::ContentTypeHeader, QStringLiteral("application/json"));
	return request;
}

Http call(const QByteArray &method, const QString &path, const QByteArray &body, const QString &token, int timeoutMs)
{
	Http out;
	QNetworkRequest request = make_request(path, token);
	QNetworkReply *reply = method == "POST" ? network().post(request, body) : network().get(request);
	QEventLoop loop;
	QTimer timer;
	timer.setSingleShot(true);
	QObject::connect(&timer, &QTimer::timeout, &loop, [&] {
		out.error = QStringLiteral("O controlador do LiveTT não respondeu a tempo.");
		reply->abort();
	});
	QObject::connect(reply, &QNetworkReply::finished, &loop, &QEventLoop::quit);
	timer.start(timeoutMs);
	loop.exec();
	timer.stop();
	out.status = reply->attribute(QNetworkRequest::HttpStatusCodeAttribute).toInt();
	out.body = reply->readAll();
	if (out.status == 0 && out.error.isEmpty())
		out.error = QStringLiteral("O controlador do LiveTT não está respondendo. Abra o painel \"LiveTT\".");
	reply->deleteLater();
	return out;
}

QString message_of(const Http &r, const QString &fallback)
{
	if (!r.error.isEmpty())
		return r.error;
	const QString text = QJsonDocument::fromJson(r.body).object().value(QStringLiteral("error")).toString();
	return text.isEmpty() ? fallback : text;
}

QString panel_token()
{
	const Http session = call("GET", QStringLiteral("/api/session"), {}, {}, 5000);
	return QJsonDocument::fromJson(session.body).object().value(QStringLiteral("token")).toString();
}

// ---- remembered choices of the start window

QString prefs_path()
{
	char *path = obs_module_config_path("native-start.json");
	const QString out = path ? QString::fromUtf8(path) : QString();
	bfree(path);
	return out;
}

QJsonObject read_prefs()
{
	QFile file(prefs_path());
	if (!file.open(QIODevice::ReadOnly))
		return {};
	return QJsonDocument::fromJson(file.readAll()).object();
}

void write_prefs(const QJsonObject &o)
{
	const QString path = prefs_path();
	if (path.isEmpty())
		return;
	QDir().mkpath(QFileInfo(path).absolutePath());
	QFile file(path);
	if (file.open(QIODevice::WriteOnly | QIODevice::Truncate))
		file.write(QJsonDocument(o).toJson());
}

// ---- the start window

struct Choice {
	QString title;
	QJsonValue minutes; // null = no limit
};

QString display_name(const QJsonObject &account)
{
	const QString name = account.value(QStringLiteral("name")).toString();
	const QString handle = account.value(QStringLiteral("handle")).toString();
	static const QRegularExpression placeholder(QStringLiteral("^user\\d{6,}$"));
	if (!name.isEmpty() && !placeholder.match(name).hasMatch())
		return name;
	return handle.isEmpty() ? QStringLiteral("Conta TikTok") : QStringLiteral("@") + handle;
}

const char *kStyle = R"(
QDialog{background:#0c0f16}
QLabel{color:#eef2f9}
QLabel#muted,QLabel#hint{color:#8d9bb3;font-size:11px}
QLabel#name{font-weight:700;font-size:14px}
QLabel#field{color:#8d9bb3;font-size:11px;font-weight:600}
QLabel#avatar{background:#222b3c;border-radius:20px;color:#eef2f9;font-weight:700;font-size:15px}
QLabel#banner{background:#2a2412;border:1px solid #5a4a1c;border-radius:10px;color:#f3c969;padding:9px 11px}
QLabel#banner[bad="true"]{background:#2a1620;border:1px solid #5a2236;color:#ffc2d0}
QLineEdit,QComboBox,QPlainTextEdit{background:#1b2230;border:1px solid #273044;border-radius:9px;color:#eef2f9;padding:7px 9px;selection-background-color:#25f4ee;selection-color:#07222a}
QLineEdit:focus,QComboBox:focus,QPlainTextEdit:focus{border:1px solid #25f4ee}
QComboBox::drop-down{border:0;width:22px}
QComboBox QAbstractItemView{background:#1b2230;color:#eef2f9;border:1px solid #273044;selection-background-color:#273044}
QPushButton{background:#1b2230;border:1px solid #273044;border-radius:10px;color:#eef2f9;padding:9px 16px;font-weight:600}
QPushButton:hover{border:1px solid #25f4ee}
QPushButton:disabled{color:#5d6b84;border:1px solid #1d2535}
QPushButton#primary{background:qlineargradient(x1:0,y1:0,x2:1,y2:0,stop:0 #fe2c55,stop:1 #ff4f81);border:0;color:#fff;font-size:14px}
QPushButton#primary:disabled{background:#222b3c;color:#5d6b84}
)";

class StartWindow : public QDialog {
public:
	StartWindow() : QDialog(main_window())
	{
		setWindowTitle(QStringLiteral("Iniciar LIVE no TikTok"));
		setModal(true);
		setMinimumWidth(480);
		setStyleSheet(QString::fromUtf8(kStyle));
		auto *root = new QVBoxLayout(this);
		root->setSpacing(10);
		root->setContentsMargins(18, 16, 18, 16);

		auto *head = new QHBoxLayout();
		avatar_ = new QLabel(QStringLiteral("?"));
		avatar_->setObjectName("avatar");
		avatar_->setFixedSize(40, 40);
		avatar_->setAlignment(Qt::AlignCenter);
		auto *who = new QVBoxLayout();
		who->setSpacing(0);
		name_ = new QLabel(QStringLiteral("Conectando…"));
		name_->setObjectName("name");
		handle_ = new QLabel();
		handle_->setObjectName("muted");
		who->addWidget(name_);
		who->addWidget(handle_);
		auto *panel = new QPushButton(QStringLiteral("Abrir painel"));
		panel->setToolTip(QStringLiteral("Contas, Sobre mim, assistente de IA e demais configurações"));
		head->addWidget(avatar_);
		head->addLayout(who, 1);
		head->addWidget(panel);
		root->addLayout(head);

		banner_ = new QLabel();
		banner_->setObjectName("banner");
		banner_->setWordWrap(true);
		banner_->hide();
		action_ = new QPushButton();
		action_->hide();
		root->addWidget(banner_);
		root->addWidget(action_, 0, Qt::AlignLeft);

		root->addWidget(label(QStringLiteral("Título")));
		title_ = new QLineEdit();
		title_->setMaxLength(80);
		title_->setText(read_prefs().value(QStringLiteral("title")).toString(QStringLiteral("Minha LIVE")));
		root->addWidget(title_);

		root->addWidget(label(QStringLiteral("Duração")));
		duration_ = new QComboBox();
		const struct {
			const char *label;
			int minutes;
		} options[] = {{"Sem limite", 0}, {"15 minutos", 15}, {"30 minutos", 30}, {"45 minutos", 45}, {"1 hora", 60},
			       {"1 h 30", 90}, {"2 horas", 120}, {"3 horas", 180}};
		const int savedMinutes = read_prefs().value(QStringLiteral("minutes")).toInt(0);
		bool savedIsPreset = false;
		for (const auto &o : options) {
			duration_->addItem(QString::fromUtf8(o.label), o.minutes);
			if (o.minutes == savedMinutes) {
				duration_->setCurrentIndex(duration_->count() - 1);
				savedIsPreset = true;
			}
		}
		duration_->addItem(QStringLiteral("Personalizado…"), -1);
		root->addWidget(duration_);

		custom_ = new QSpinBox();
		custom_->setRange(1, 1440);
		custom_->setSuffix(QStringLiteral(" min"));
		custom_->setValue(savedMinutes > 0 ? savedMinutes : 20);
		root->addWidget(custom_);
		auto showCustom = [this] { custom_->setVisible(duration_->currentData().toInt() < 0); };
		if (!savedIsPreset && savedMinutes > 0)
			duration_->setCurrentIndex(duration_->count() - 1);
		showCustom();
		QObject::connect(duration_, QOverload<int>::of(&QComboBox::currentIndexChanged), this, showCustom);

		auto *note = new QLabel(QStringLiteral("A cena e o áudio do OBS vão ao ar no TikTok e ficam visíveis ao público. O contexto para a IA, as contas e os demais ajustes ficam no painel."));
		note->setObjectName("hint");
		note->setWordWrap(true);
		root->addWidget(note);

		auto *row = new QHBoxLayout();
		row->addStretch(1);
		auto *cancel = new QPushButton(QStringLiteral("Cancelar"));
		start_ = new QPushButton(QStringLiteral("Iniciar LIVE"));
		start_->setObjectName("primary");
		start_->setDefault(true);
		start_->setMinimumWidth(170);
		start_->setEnabled(false);
		row->addWidget(cancel);
		row->addWidget(start_);
		root->addLayout(row);

		QObject::connect(cancel, &QPushButton::clicked, this, &QDialog::reject);
		QObject::connect(start_, &QPushButton::clicked, this, &QDialog::accept);
		QObject::connect(panel, &QPushButton::clicked, this, [this] {
			reject();
			tiktok_show_panel();
		});
		QObject::connect(title_, &QLineEdit::textChanged, this, [this] { update_start(); });
		QObject::connect(action_, &QPushButton::clicked, this, [this] { connect_account(); });
		poll_ = new QTimer(this);
		QObject::connect(poll_, &QTimer::timeout, this, [this] { refresh(); });
		poll_->start(2000);
		refresh();
	}

	Choice choice() const
	{
		Choice c;
		c.title = title_->text().trimmed();
		int minutes = duration_->currentData().toInt();
		if (minutes < 0)
			minutes = custom_->value();
		c.minutes = minutes > 0 ? QJsonValue(minutes) : QJsonValue();
		return c;
	}

private:
	static QLabel *label(const QString &text)
	{
		auto *l = new QLabel(text);
		l->setObjectName("field");
		return l;
	}

	bool ready() const { return phase_ == QLatin1String("ready") || phase_ == QLatin1String("closed"); }
	void update_start() { start_->setEnabled(ready() && !title_->text().trimmed().isEmpty()); }

	void banner(const QString &text, bool bad, const QString &actionText = QString())
	{
		banner_->setText(text);
		banner_->setProperty("bad", bad);
		banner_->style()->unpolish(banner_);
		banner_->style()->polish(banner_);
		banner_->setVisible(!text.isEmpty());
		action_->setText(actionText);
		action_->setVisible(!actionText.isEmpty());
	}

	void refresh()
	{
		QNetworkReply *reply = network().get(make_request(QStringLiteral("/api/state"), {}));
		QObject::connect(reply, &QNetworkReply::finished, this, [this, reply] {
			reply->deleteLater();
			const int status = reply->attribute(QNetworkRequest::HttpStatusCodeAttribute).toInt();
			if (status != 200) {
				phase_ = QStringLiteral("unreachable");
				banner(QStringLiteral("O controlador do LiveTT não está respondendo. Abra o painel e tente de novo."), true);
				update_start();
				return;
			}
			apply(QJsonDocument::fromJson(reply->readAll()).object());
		});
	}

	void apply(const QJsonObject &state)
	{
		phase_ = state.value(QStringLiteral("phase")).toString();
		QJsonObject active;
		for (const QJsonValue &v : state.value(QStringLiteral("accounts")).toArray())
			if (v.toObject().value(QStringLiteral("active")).toBool())
				active = v.toObject();
		if (!active.isEmpty()) {
			name_->setText(display_name(active));
			const QString handle = active.value(QStringLiteral("handle")).toString();
			handle_->setText(handle.isEmpty() ? QString() : QStringLiteral("@") + handle);
			const QString photoUrl = active.value(QStringLiteral("avatar")).toString();
			if (!havePhoto_ || photoUrl != avatarUrl_) { // a refresh must not wipe a photo that is already showing
				havePhoto_ = false;
				avatar_->setPixmap(QPixmap());
				avatar_->setText(display_name(active).remove(0, display_name(active).startsWith('@') ? 1 : 0).left(1).toUpper());
			}
			load_avatar(photoUrl);
		} else {
			name_->setText(QStringLiteral("Nenhuma conta conectada"));
			handle_->clear();
			havePhoto_ = false;
			avatarUrl_.clear();
			avatar_->setPixmap(QPixmap());
			avatar_->setText(QStringLiteral("?"));
		}
		const QString detail = state.value(QStringLiteral("detail")).toString();
		if (ready()) {
			banner({}, false);
		} else if (phase_ == QLatin1String("disconnected")) {
			banner(QStringLiteral("Conectando ao OBS e ao TikTok…"), false);
			if (!connecting_) {
				connecting_ = true;
				connect_account();
			}
		} else if (phase_ == QLatin1String("login")) {
			banner(QStringLiteral("Nenhuma conta do TikTok conectada. Entre na janela de login que abriu (a janela fecha sozinha quando terminar)."), false,
			       QStringLiteral("Abrir o login de novo"));
		} else if (phase_ == QLatin1String("creating") || phase_ == QLatin1String("live") || phase_ == QLatin1String("ending")) {
			banner(QStringLiteral("Já existe uma LIVE em andamento. Use o painel para acompanhá-la ou encerrá-la."), true);
		} else {
			banner(detail.isEmpty() ? QStringLiteral("O LiveTT ainda não está pronto.") : detail, true);
		}
		update_start();
	}

	// "Conectar" is read-only: it checks OBS and the TikTok session, and opens the sign-in window when there is none.
	void connect_account()
	{
		const QString token = panel_token();
		if (token.isEmpty())
			return;
		QNetworkReply *reply = network().post(make_request(QStringLiteral("/api/connect"), token), "{}");
		QObject::connect(reply, &QNetworkReply::finished, this, [this, reply] {
			reply->deleteLater();
			refresh();
		});
	}

	void load_avatar(const QString &url)
	{
		if (url.isEmpty() || url == avatarUrl_)
			return;
		avatarUrl_ = url;
		const QString path = QStringLiteral("/avatar?u=") + QString::fromUtf8(QUrl::toPercentEncoding(url));
		QNetworkReply *reply = network().get(make_request(path, {}));
		QObject::connect(reply, &QNetworkReply::finished, this, [this, reply] {
			reply->deleteLater();
			QPixmap photo;
			const QByteArray bytes = reply->readAll();
			const bool decoded = reply->error() == QNetworkReply::NoError && photo.loadFromData(bytes);
			if (!decoded)
				return;
			photo = photo.scaled(40, 40, Qt::KeepAspectRatioByExpanding, Qt::SmoothTransformation);
			QPixmap round(40, 40);
			round.fill(Qt::transparent);
			QPainter painter(&round);
			painter.setRenderHint(QPainter::Antialiasing);
			QPainterPath clip;
			clip.addEllipse(0, 0, 40, 40);
			painter.setClipPath(clip);
			painter.drawPixmap(0, 0, photo);
			avatar_->setText(QString());
			avatar_->setPixmap(round);
			havePhoto_ = true;
		});
	}

	QLabel *avatar_, *name_, *handle_, *banner_;
	QPushButton *action_, *start_;
	QLineEdit *title_;
	QComboBox *duration_;
	QSpinBox *custom_;
	QTimer *poll_;
	QString phase_, avatarUrl_;
	bool connecting_ = false;
	bool havePhoto_ = false;
};

// ---- the flow

// What the user confirmed. Valid only for a few seconds: it exists so the OBS button can ask BEFORE OBS starts
// (cancelling is then silent), and a start that happens later must ask again.
struct Pending {
	bool valid = false;
	qint64 at = 0;
	QString scene;
	Choice choice;
};
Pending g_pending;

constexpr qint64 kPendingMs = 10000;

// Shows the start window. True = "Iniciar LIVE" was clicked (the scene is read fresh, after the window closed).
bool ask_user(Pending &out)
{
	StartWindow window;
	if (window.exec() != QDialog::Accepted)
		return false;
	out.choice = window.choice();
	write_prefs(QJsonObject{{"title", out.choice.title},
				{"minutes", out.choice.minutes.isNull() ? 0 : out.choice.minutes.toInt()}});
	const Http state = call("GET", QStringLiteral("/api/state"), {}, {}, 8000);
	out.scene = QJsonDocument::fromJson(state.body).object().value(QStringLiteral("obs")).toObject().value(QStringLiteral("scene")).toString();
	out.valid = true;
	out.at = QDateTime::currentMSecsSinceEpoch();
	return true;
}

// Asks the controller to create the room and give us the destination, while a small window says what is going on.
bool create_live(const Pending &p, QString &error)
{
	QDialog busy(main_window());
	busy.setWindowTitle(QStringLiteral("LiveTT"));
	busy.setModal(true);
	busy.setStyleSheet(QString::fromUtf8(kStyle));
	busy.setWindowFlag(Qt::WindowCloseButtonHint, false);
	auto *column = new QVBoxLayout(&busy);
	column->addWidget(new QLabel(QStringLiteral("Criando a LIVE no TikTok…")));
	auto *bar = new QProgressBar();
	bar->setRange(0, 0);
	column->addWidget(bar);
	busy.setMinimumWidth(340);
	busy.show();

	const QString token = panel_token();
	if (token.isEmpty()) {
		error = QStringLiteral("Não foi possível falar com o controlador do LiveTT.");
		return false;
	}
	QJsonObject request{{"title", p.choice.title}, {"scene", p.scene}, {"confirmed", true}, {"maxMinutes", p.choice.minutes}};
	const Http reply = call("POST", QStringLiteral("/api/start-native"), QJsonDocument(request).toJson(QJsonDocument::Compact), token, 110000);
	busy.close();
	if (reply.status != 200) {
		error = message_of(reply, QStringLiteral("Não foi possível criar a LIVE."));
		return false;
	}
	const QJsonObject target = QJsonDocument::fromJson(reply.body).object();
	const std::string server = target.value(QStringLiteral("server")).toString().toStdString();
	const std::string key = target.value(QStringLiteral("key")).toString().toStdString();
	if (server.empty() || key.empty()) {
		error = QStringLiteral("O TikTok não devolveu o destino da transmissão.");
		return false;
	}
	set_target(server, key);
	return true;
}

// Returns true when the controller handed the destination over and OBS may start. On false, `error` says why
// (empty = the user cancelled).
bool native_start(QString &error)
{
	Pending p;
	if (g_pending.valid && QDateTime::currentMSecsSinceEpoch() - g_pending.at < kPendingMs) {
		p = g_pending; // the OBS button already asked
	} else if (!ask_user(p)) {
		error.clear();
		return false;
	}
	g_pending = Pending();
	return create_live(p, error);
}

// ---- the OBS button, for this start only
//
// OBS saves the streaming service only when Settings or the frontend API's "save" is used, so putting the plugin's service
// in OBS's memory for the duration of a start never touches the profile's service.json: the profile keeps whatever the user
// had (or nothing), and with the plugin disabled OBS behaves exactly as before.

OBSService g_ours;
OBSService g_previous;
bool g_swapped = false;

void swap_in()
{
	if (g_swapped)
		return;
	obs_service_t *current = obs_frontend_get_streaming_service();
	if (current && std::string(obs_service_get_type(current)) == kServiceId)
		return; // already the plugin's (a profile left that way by an older version)
	OBSDataAutoRelease settings = obs_data_create();
	OBSServiceAutoRelease created = obs_service_create(kServiceId, "default_service", settings, nullptr);
	if (!created)
		return;
	g_ours = created.Get();
	g_previous = current;
	obs_frontend_set_streaming_service(g_ours); // memory only: obs_frontend_save_streaming_service is never called
	g_swapped = true;
}

void swap_out()
{
	if (g_swapped && g_previous)
		obs_frontend_set_streaming_service(g_previous);
	g_previous = nullptr;
	g_ours = nullptr;
	g_swapped = false;
}

QPushButton *start_button()
{
	QWidget *main = main_window();
	return main ? main->findChild<QPushButton *>(QStringLiteral("streamButton")) : nullptr;
}

// The whole start: ask, take the button over, press it again. Returns without waiting for the stream.
void start_flow()
{
	QPushButton *button = start_button();
	if (!button || g_starting || obs_frontend_streaming_active())
		return;
	g_starting = true;
	Pending p;
	const bool yes = ask_user(p);
	g_starting = false;
	if (!yes)
		return;
	g_pending = p;
	swap_in();
	// The modal window cost the button its "pressed" state: press it again from the event loop (OBS runs its usual checks).
	QTimer::singleShot(0, button, [button] {
		if (button->isEnabled())
			button->click();
	});
	// If OBS never got as far as asking the service for the destination, give the profile its own service back.
	QTimer::singleShot(45000, qApp, [] {
		if (g_swapped && !has_target() && !obs_frontend_streaming_active())
			swap_out();
	});
}

class StartButtonFilter : public QObject {
public:
	using QObject::QObject;

protected:
	bool eventFilter(QObject *object, QEvent *event) override
	{
		auto *button = qobject_cast<QPushButton *>(object);
		if (!button || !button->isEnabled())
			return false;
		const bool released = event->type() == QEvent::MouseButtonRelease && static_cast<QMouseEvent *>(event)->button() == Qt::LeftButton &&
				      button->rect().contains(static_cast<QMouseEvent *>(event)->position().toPoint());
		const auto key = event->type() == QEvent::KeyRelease ? static_cast<QKeyEvent *>(event)->key() : 0;
		const bool keyed = key == Qt::Key_Space || key == Qt::Key_Return || key == Qt::Key_Enter;
		if (!released && !keyed)
			return false;
		if (obs_frontend_streaming_active() || g_starting)
			return false; // a stop, or a start under way: OBS does what it always did
		button->setDown(false);
		start_flow();
		return true; // the click is swallowed: the plugin presses the button itself once the user said yes
	}
};

QPointer<StartButtonFilter> g_filter;

// ---- obs_service_info callbacks

struct ServiceData {
	obs_service_t *service;
};

const char *service_name(void *) { return "LiveTT"; }

void *service_create(obs_data_t *, obs_service_t *service) { return new ServiceData{service}; }

void service_destroy(void *data) { delete static_cast<ServiceData *>(data); }

void service_update(void *, obs_data_t *) {}

obs_properties_t *service_properties(void *)
{
	obs_properties_t *props = obs_properties_create();
	obs_properties_add_text(props, "info", "A LIVE é criada pelo plugin LiveTT quando você inicia a transmissão.", OBS_TEXT_INFO);
	return props;
}

bool service_can_try_to_connect(void *) { return true; } // the destination is created when the stream starts

bool service_initialize(void *, obs_output_t *output)
{
	if (QThread::currentThread() != QApplication::instance()->thread()) {
		obs_output_set_last_error(output, "Inicie a LIVE pelo botão do OBS ou pelo painel LiveTT.");
		return false;
	}
	if (g_starting) {
		obs_output_set_last_error(output, "Já existe um início de LIVE em andamento.");
		return false;
	}
	g_starting = true;
	QString error;
	const bool ok = native_start(error);
	g_starting = false;
	if (!ok) {
		if (error.isEmpty())
			error = QStringLiteral("Transmissão cancelada.");
		const QByteArray text = error.toUtf8();
		obs_output_set_last_error(output, text.constData());
		set_target({}, {});
		QTimer::singleShot(0, qApp, [] { swap_out(); }); // after OBS has finished handling this failure
	}
	return ok;
}

const char *service_protocol(void *) { return "RTMP"; }

const char *service_output_type(void *) { return "rtmp_output"; }

const char *service_connect_info(void *, uint32_t type)
{
	thread_local std::string copy;
	std::lock_guard<std::mutex> lock(g_mutex);
	switch (type) {
	case OBS_SERVICE_CONNECT_INFO_SERVER_URL:
		copy = g_server;
		return copy.c_str();
	case OBS_SERVICE_CONNECT_INFO_STREAM_KEY:
		copy = g_key;
		return copy.c_str();
	default:
		return nullptr;
	}
}

} // namespace

void tiktok_service_register()
{
	static struct obs_service_info info = {};
	info.id = kServiceId;
	info.get_name = service_name;
	info.create = service_create;
	info.destroy = service_destroy;
	info.update = service_update;
	info.get_properties = service_properties;
	info.initialize = service_initialize;
	info.can_try_to_connect = service_can_try_to_connect;
	info.get_protocol = service_protocol;
	info.get_output_type = service_output_type;
	info.get_connect_info = service_connect_info;
	obs_register_service(&info);
}

void tiktok_service_watch_start_button()
{
	QPushButton *button = start_button();
	if (!button) {
		blog(LOG_WARNING, "[livett] OBS start button not found: the start window will open only from the panel");
		return;
	}
	if (!g_filter)
		g_filter = new StartButtonFilter(button);
	button->installEventFilter(g_filter);
}

bool tiktok_service_start_in_progress() { return g_starting || g_swapped; }

void tiktok_service_request_start()
{
	QTimer::singleShot(0, qApp, [] { start_flow(); });
}

void tiktok_service_on_frontend_event(enum obs_frontend_event event)
{
	switch (event) {
	case OBS_FRONTEND_EVENT_STREAMING_STOPPED:
		set_target({}, {}); // the key lives in memory only, and only while the stream runs
		swap_out();
		break;
	case OBS_FRONTEND_EVENT_PROFILE_CHANGED:
	case OBS_FRONTEND_EVENT_EXIT:
		g_previous = nullptr; // the frontend reloaded (or is dropping) its own service: nothing of ours to give back
		g_ours = nullptr;
		g_swapped = false;
		break;
	default:
		break;
	}
}
