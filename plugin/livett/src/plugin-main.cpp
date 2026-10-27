// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// LiveTT: native docks + controller launcher.
//
// This module does not talk to TikTok itself. It (1) starts the local controller
// (data/controller/controller.exe serve --background) if it is not running, (2) adds two docks that embed the
// controller's web UI through OBS's own browser panel API, and (3) adds a Tools-menu entry that opens
// the live feed (stats + chat) in a floating, always-on-top window. The controller keeps running when
// OBS closes on purpose: a broadcast must never depend on the OBS UI lifetime.
#include <obs-module.h>
#include <obs-frontend-api.h>
#include <util/platform.h>
#include <browser-panel.hpp>

#include "obs-api.hpp"
#include "obs-service.hpp"
#include "phone-camera.hpp"

#include <QDateTime>
#include <QHash>
#include <QLocale>
#include <QNetworkAccessManager>
#include <QNetworkReply>
#include <QNetworkRequest>
#include <QLinearGradient>
#include <QPainter>
#include <QPainterPath>
#include <QPixmap>
#include <QSet>
#include <QUrl>
#include <QVector>
#include <QWheelEvent>
#include <QCloseEvent>
#include <QHBoxLayout>
#include <QMouseEvent>
#include <QSizeGrip>
#include <QSlider>
#include <QStyle>
#include <QToolButton>
#include <QWindow>
#include <windows.h>
#include <QDir>
#include <QFileInfo>
#include <QAction>
#include <QDockWidget>
#include <QMainWindow>
#include <QFile>
#include <QJsonDocument>
#include <QJsonObject>
#include <QLabel>
#include <QHash>
#include <QLineEdit>
#include <QMenu>
#include <QProcess>
#include <QJsonArray>
#include <QPointer>
#include <QTcpSocket>
#include <QTimer>
#include <QVBoxLayout>
#include <QWidget>

OBS_DECLARE_MODULE()
OBS_MODULE_USE_DEFAULT_LOCALE("livett", "en-US")

MODULE_EXPORT const char *obs_module_name(void) { return "LiveTT"; }
MODULE_EXPORT const char *obs_module_description(void) { return "LiveTT control panel, live feed and controller launcher"; }

namespace {

constexpr const char *kControlId = "livett-control";
constexpr quint16 kPort = 8765;
// Controller address. TIKTOK_PLUGIN_BASE exists only to point the plugin at a test stub.
const QString &base()
{
	static const QString value = qEnvironmentVariableIsSet("TIKTOK_PLUGIN_BASE") ? qEnvironmentVariable("TIKTOK_PLUGIN_BASE")
										       : QStringLiteral("http://127.0.0.1:8765");
	return value;
}

QPointer<QWidget> g_floating;
QTimer *g_wait = nullptr;
int g_wait_ticks = 0;
bool g_docks_added = false;

bool controller_listening()
{
	QTcpSocket socket;
	socket.connectToHost(QStringLiteral("127.0.0.1"), kPort);
	const bool ok = socket.waitForConnected(250);
	socket.abort();
	return ok;
}

QString config_path()
{
	char *path = obs_module_get_config_path(obs_current_module(), "config.json");
	if (!path)
		return {};
	const QString result = QString::fromUtf8(path);
	bfree(path);
	return result;
}

// Plugin settings (dock layout, floating window); written by the plugin itself.
QJsonObject read_config()
{
	QFile file(config_path());
	if (config_path().isEmpty() || !file.open(QIODevice::ReadOnly))
		return {};
	return QJsonDocument::fromJson(file.readAll()).object();
}

void write_config(const QJsonObject &object)
{
	const QString path = config_path();
	if (path.isEmpty())
		return;
	QDir().mkpath(QFileInfo(path).absolutePath());
	QFile file(path);
	if (file.open(QIODevice::WriteOnly | QIODevice::Truncate))
		file.write(QJsonDocument(object).toJson());
}

// The controller is the single-file Go executable bundled with the plugin: <plugins>/livett/data/controller/controller.exe.
struct ControllerLaunch {
	QString program;
	QStringList args;
	QString dir;
};

bool resolve_controller(ControllerLaunch &out)
{
	char *bundled_dir = obs_module_file("controller");
	const QString controller_dir = bundled_dir ? QString::fromUtf8(bundled_dir) : QString();
	bfree(bundled_dir);
	if (controller_dir.isEmpty())
		return false;
	const QString exe = QDir(controller_dir).filePath(QStringLiteral("controller.exe"));
	if (!QFile::exists(exe))
		return false;
	out.program = exe;
	out.args = {QStringLiteral("serve"), QStringLiteral("--background"), QStringLiteral("--exit-with-obs"), QStringLiteral("--root"),
		    controller_dir};
	out.dir = controller_dir;
	return true;
}

bool start_controller()
{
	ControllerLaunch launch;
	if (!resolve_controller(launch)) {
		blog(LOG_WARNING, "[livett] controller.exe not found next to the plugin (data/controller); reinstall the package");
		return false;
	}
	// CreateProcess with bInheritHandles = FALSE: a detached child that inherited OBS's sockets would keep the
	// obs-websocket port (4455) bound after OBS exits, and the next OBS could not start its WebSocket server.
	const std::wstring exe = QDir::toNativeSeparators(launch.program).toStdWString();
	const std::wstring dir = QDir::toNativeSeparators(launch.dir).toStdWString();
	std::wstring command = L"\"" + exe + L"\"";
	for (const QString &arg : launch.args)
		command += L" \"" + QDir::toNativeSeparators(arg).toStdWString() + L"\"";
	STARTUPINFOW startup{};
	startup.cb = sizeof(startup);
	PROCESS_INFORMATION process{};
	const bool started = CreateProcessW(exe.c_str(), command.data(), nullptr, nullptr, FALSE,
					    DETACHED_PROCESS | CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP, nullptr, dir.c_str(), &startup,
					    &process) != 0;
	if (started) {
		CloseHandle(process.hThread);
		CloseHandle(process.hProcess);
	}
	blog(LOG_INFO, "[livett] controller start requested (%s): %s", launch.program.toUtf8().constData(),
	     started ? "ok" : "failed");
	return started;
}

QCef *browser_api()
{
	static QCef *cef = obs_browser_init_panel();
	if (cef && !cef->initialized())
		cef->init_browser();
	return cef;
}

QWidget *make_web_widget(const QString &path)
{
	QCef *cef = browser_api();
	if (!cef || !cef->wait_for_browser_init()) {
		auto *label = new QLabel(QObject::tr("O navegador embutido do OBS não está disponível."));
		label->setWordWrap(true);
		label->setMargin(12);
		return label;
	}
	QCefWidget *widget = cef->create_widget(nullptr, (base() + path).toStdString());
	if (widget) {
		widget->setAttribute(Qt::WA_NativeWindow);
		widget->allowAllPopups(false);
	}
	return widget;
}

// Title text that moves the frameless window.
class DragLabel : public QLabel {
public:
	explicit DragLabel(const QString &text, QWidget *parent = nullptr) : QLabel(text, parent)
	{
		setCursor(Qt::SizeAllCursor);
	}

protected:
	void mousePressEvent(QMouseEvent *event) override
	{
		if (event->button() == Qt::LeftButton && window()->windowHandle())
			window()->windowHandle()->startSystemMove();
	}
};

// Native floating live feed (stats + chat) drawn with Qt using per-pixel alpha.
// Only the BACKGROUND (cards, bubbles, bar) is translucent, controlled by the slider; text, numbers and photos are
// always fully opaque (drawn with a soft shadow so they stay readable over any image). Data comes from the
// controller's read-only /api/state, so it works without the embedded browser.
struct FeedItem {
	qint64 n = 0;
	QString kind, name, text, gift, rank, avatar;
	int count = 0, coins = 0;
};

class FloatingOverlay : public QWidget {
public:
	FloatingOverlay() : QWidget(nullptr, Qt::Window | Qt::FramelessWindowHint)
	{
		setWindowTitle(QStringLiteral("LiveTT · Ao vivo"));
		setObjectName(QStringLiteral("floatoverlay"));
		setAttribute(Qt::WA_TranslucentBackground);
		setAttribute(Qt::WA_DeleteOnClose);
		setMouseTracking(true);
		setMinimumSize(190 + 2 * kShadow, 260 + 2 * kShadow);

		const QJsonObject saved = read_config().value(QStringLiteral("overlay")).toObject();
		bg_ = qBound(0, saved.value(QStringLiteral("bg")).toInt(45), 100);
		const bool on_top = saved.value(QStringLiteral("on_top")).toBool(true);

		setStyleSheet(QStringLiteral(
			"QLabel{color:#eef2f9;font-weight:600;background:transparent}"
			"QToolButton{color:#eef2f9;background:#1b2230;border:0;padding:3px 8px;border-radius:7px}"
			"QToolButton:hover{background:#2d3850}QToolButton:checked{background:#25f4ee;color:#07222a}"
			"QSlider::groove:horizontal{height:4px;background:#4a5873;border-radius:2px}"
			"QSlider::handle:horizontal{width:12px;margin:-5px 0;border-radius:6px;background:#25f4ee}"));

		bar_ = new QWidget(this);
		bar_->setAttribute(Qt::WA_TranslucentBackground);
		auto *row = new QHBoxLayout(bar_);
		row->setContentsMargins(10, 4, 6, 4);
		row->setSpacing(4);
		auto *title = new DragLabel(QStringLiteral("⠿ LiveTT"));
		title->setSizePolicy(QSizePolicy::Ignored, QSizePolicy::Preferred);
		title->setMinimumWidth(0);
		row->addWidget(title, 1);
		slider_ = new QSlider(Qt::Horizontal);
		slider_->setRange(0, 100);
		slider_->setValue(bg_);
		slider_->setFixedWidth(70);
		slider_->setToolTip(QStringLiteral("Opacidade do fundo (o texto fica sempre 100% visível)"));
		row->addWidget(slider_);
		pin_ = new QToolButton();
		pin_->setText(QStringLiteral("Fixar"));
		pin_->setCheckable(true);
		pin_->setChecked(on_top);
		pin_->setToolTip(QStringLiteral("Manter sempre por cima das outras janelas"));
		row->addWidget(pin_);
		auto *close = new QToolButton();
		close->setText(QStringLiteral("✕"));
		row->addWidget(close);

		const int w = saved.value(QStringLiteral("w")).toInt(380 + 2 * kShadow), h = saved.value(QStringLiteral("h")).toInt(640 + 2 * kShadow);
		resize(qMax(190 + 2 * kShadow, w), qMax(260 + 2 * kShadow, h));
		if (saved.contains(QStringLiteral("x")))
			move(saved.value(QStringLiteral("x")).toInt(), saved.value(QStringLiteral("y")).toInt());

		net_ = new QNetworkAccessManager(this);

		// Host reply box: same /api/chat the panel uses (live only, rate limited, no links, no repeats).
		send_ = new QWidget(this);
		auto *send_col = new QVBoxLayout(send_);
		send_col->setContentsMargins(0, 0, 0, 0);
		send_col->setSpacing(2);
		auto *send_row = new QHBoxLayout();
		send_row->setSpacing(6);
		input_ = new QLineEdit();
		input_->setMaxLength(200);
		input_->setStyleSheet(QStringLiteral("QLineEdit{color:#eef2f9;background:#1b2230;border:1px solid #273044;border-radius:8px;"
						     "padding:6px 8px;selection-background-color:#25f4ee;selection-color:#07222a}"
						     "QLineEdit:focus{border:1px solid #25f4ee}QLineEdit:disabled{color:#8d9bb3}"));
		// Qt's built-in menu is English and this plugin ships no translations: offer the same actions in Portuguese.
		input_->setContextMenuPolicy(Qt::CustomContextMenu);
		QObject::connect(input_, &QWidget::customContextMenuRequested, this, [this](const QPoint &pos) {
			QMenu menu(input_);
			const bool editable = !input_->isReadOnly() && input_->isEnabled();
			QAction *cut = menu.addAction(QStringLiteral("Recortar"), input_, &QLineEdit::cut);
			QAction *copy = menu.addAction(QStringLiteral("Copiar"), input_, &QLineEdit::copy);
			QAction *paste = menu.addAction(QStringLiteral("Colar"), input_, &QLineEdit::paste);
			menu.addSeparator();
			QAction *all = menu.addAction(QStringLiteral("Selecionar tudo"), input_, &QLineEdit::selectAll);
			cut->setEnabled(editable && input_->hasSelectedText());
			copy->setEnabled(input_->hasSelectedText());
			paste->setEnabled(editable);
			all->setEnabled(!input_->text().isEmpty());
			menu.exec(input_->mapToGlobal(pos));
		});
		send_btn_ = new QToolButton();
		send_btn_->setText(QStringLiteral("Enviar"));
		send_row->addWidget(input_, 1);
		send_row->addWidget(send_btn_);
		send_status_ = new QLabel();
		send_status_->setStyleSheet(QStringLiteral("QLabel{font-size:11px;font-weight:500;color:#8d9bb3}"));
		send_col->addLayout(send_row);
		send_col->addWidget(send_status_);
		set_send_enabled(false);
		QObject::connect(input_, &QLineEdit::returnPressed, this, [this] { send_message(); });
		QObject::connect(send_btn_, &QToolButton::clicked, this, [this] { send_message(); });
		QObject::connect(slider_, &QSlider::valueChanged, this, [this](int value) { bg_ = value; update(); });
		QObject::connect(pin_, &QToolButton::toggled, this, [this](bool) { apply_topmost(); });
		QObject::connect(close, &QToolButton::clicked, this, &QWidget::close);
		auto *poll = new QTimer(this);
		QObject::connect(poll, &QTimer::timeout, this, [this] { fetch_state(); });
		poll->start(1500);
		auto *tick = new QTimer(this);
		QObject::connect(tick, &QTimer::timeout, this, [this] { update(); });
		tick->start(1000);
		fetch_state();
	}

	void apply_topmost()
	{
		SetWindowPos(reinterpret_cast<HWND>(winId()), pin_->isChecked() ? HWND_TOPMOST : HWND_NOTOPMOST, 0, 0, 0, 0,
			     SWP_NOMOVE | SWP_NOSIZE | SWP_NOACTIVATE);
	}

protected:
	void showEvent(QShowEvent *event) override
	{
		QWidget::showEvent(event);
		apply_topmost();
	}

	void resizeEvent(QResizeEvent *event) override
	{
		QWidget::resizeEvent(event);
		bar_->setGeometry(kShadow + kPad, kShadow + kPad, width() - 2 * kShadow - 2 * kPad, 34);
		send_->setGeometry(kShadow + kPad, height() - kShadow - kPad - kSendRoom + 4, width() - 2 * kShadow - 2 * kPad, kSendRoom - 4);
	}

	void closeEvent(QCloseEvent *event) override
	{
		QJsonObject config = read_config();
		QJsonObject saved;
		saved.insert(QStringLiteral("bg"), bg_);
		saved.insert(QStringLiteral("on_top"), pin_->isChecked());
		saved.insert(QStringLiteral("x"), x());
		saved.insert(QStringLiteral("y"), y());
		saved.insert(QStringLiteral("w"), width());
		saved.insert(QStringLiteral("h"), height());
		config.insert(QStringLiteral("overlay"), saved);
		write_config(config);
		QWidget::closeEvent(event);
	}

	void wheelEvent(QWheelEvent *event) override
	{
		const int before = scroll_;
		scroll_ = qBound(0, scroll_ + event->angleDelta().y() / 2, static_cast<int>(max_scroll_));  // up = older
		blog(LOG_INFO, "[livett] wheel delta=%d scroll %d -> %d (max %.0f)", event->angleDelta().y(), before, scroll_, max_scroll_);
		event->accept();
		update();
	}

	void mouseMoveEvent(QMouseEvent *event) override
	{
		const QPointF pos = event->position() - QPointF(kShadow, kShadow);
		if (dragging_) {
			set_scroll_from_track(pos.y());
			return;
		}
		if (track_.isValid() && track_.adjusted(-8, 0, 8, 0).contains(pos) || jump_.contains(pos))
			setCursor(Qt::ArrowCursor);
		else
			setCursor(cursor_for(edges_at(event->position().toPoint())));
		QWidget::mouseMoveEvent(event);
	}

	void mousePressEvent(QMouseEvent *event) override
	{
		const QPointF pos = event->position() - QPointF(kShadow, kShadow);
		if (event->button() == Qt::LeftButton) {
			if (jump_.isValid() && jump_.contains(pos)) {
				scroll_ = 0;
				update();
				return;
			}
			if (track_.isValid() && track_.adjusted(-8, 0, 8, 0).contains(pos)) {
				dragging_ = true;
				set_scroll_from_track(pos.y());
				return;
			}
			const Qt::Edges edges = edges_at(event->position().toPoint());
			if (edges && windowHandle()) {
				windowHandle()->startSystemResize(edges);
				return;
			}
		}
		QWidget::mousePressEvent(event);
	}

	void mouseReleaseEvent(QMouseEvent *event) override
	{
		if (dragging_) {
			dragging_ = false;
			update();
		}
		QWidget::mouseReleaseEvent(event);
	}

	void paintEvent(QPaintEvent *) override
	{
		QPainter p(this);
		p.setRenderHints(QPainter::Antialiasing | QPainter::TextAntialiasing | QPainter::SmoothPixmapTransform);
		// Soft shadow inside the transparent margin, then the window itself: a translucent (never fully
		// transparent) dark layer with a visible outline. Cards and bubbles add their own denser fill on top.
		const int W = width() - 2 * kShadow, H = height() - 2 * kShadow;
		const QRectF win(kShadow, kShadow, W, H);
		p.setPen(Qt::NoPen);
		for (int i = kShadow; i >= 1; --i) {
			const double fade = 1.0 - static_cast<double>(i) / (kShadow + 1);
			p.setBrush(QColor(0, 0, 0, static_cast<int>(16 * fade * fade) + 1));
			p.drawRoundedRect(win.adjusted(-i, -i, i, i), 14 + i, 14 + i);
		}
		const int window_alpha = 14 + bg_ * 150 / 100;
		p.setPen(QPen(QColor(255, 255, 255, 92), 1.2));
		p.setBrush(QColor(8, 11, 18, window_alpha));
		p.drawRoundedRect(win.adjusted(0.5, 0.5, -0.5, -0.5), 14, 14);
		p.translate(kShadow, kShadow);
		p.setClipRect(QRectF(0, 0, W, H));

		const int alpha = 2 + bg_ * 230 / 100;                   // background only: 2..232
		const int line = qMin(255, 55 + alpha / 2);              // borders follow the background
		const QColor fill(12, 16, 24, alpha), edge(74, 88, 115, line);
		auto panel = [&](const QRectF &r, qreal radius, const QColor &border) {
			p.setPen(QPen(border, 1));
			p.setBrush(fill);
			p.drawRoundedRect(r, radius, radius);
		};
		const QColor soft(255, 255, 255, 38);  // hairline used by the bar, the badge and the chat messages
		// Block = translucent fill + the teal-to-pink tint of the "Agora" card + the blue-gray border of the web layout.
		auto tinted = [&](const QRectF &r, qreal radius, const QColor &border, qreal strength) {
			panel(r, radius, Qt::transparent);
			QLinearGradient tint(r.left(), 0, r.right(), 0);
			tint.setColorAt(0.0, QColor(37, 244, 238, static_cast<int>((24 + bg_ * 30 / 100) * strength)));
			tint.setColorAt(0.65, QColor(254, 44, 85, static_cast<int>((14 + bg_ * 22 / 100) * strength)));
			tint.setColorAt(1.0, QColor(254, 44, 85, 0));
			p.setPen(Qt::NoPen);
			p.setBrush(tint);
			p.drawRoundedRect(r, radius, radius);
			p.setPen(QPen(border, 1));
			p.setBrush(Qt::NoBrush);
			p.drawRoundedRect(r, radius, radius);
		};

		int y = kPad;
		panel(QRectF(kPad, y, W - 2 * kPad, 34), 10, soft);  // control bar (its widgets sit on top)
		y += 34 + 8;

		// ---- status line
		const bool live = phase_ == QLatin1String("live") || phase_ == QLatin1String("ending");
		static const QHash<QString, QString> phase_names = {
			{QStringLiteral("ready"), QStringLiteral("PRONTO")},       {QStringLiteral("creating"), QStringLiteral("INICIANDO")},
			{QStringLiteral("closed"), QStringLiteral("ENCERRADA")},   {QStringLiteral("login"), QStringLiteral("ENTRAR NO TIKTOK")},
			{QStringLiteral("uncertain"), QStringLiteral("VERIFICAR")}, {QStringLiteral("recovery"), QStringLiteral("RECUPERAÇÃO")}};
		const QString badge = live ? QStringLiteral("AO VIVO") : phase_names.value(phase_, QStringLiteral("OFFLINE"));
		QFont badge_font = font();
		badge_font.setPointSizeF(9);
		badge_font.setBold(true);
		p.setFont(badge_font);
		const QFontMetrics bfm(badge_font);
		const QRectF badge_rect(kPad, y, bfm.horizontalAdvance(badge) + 36, 26);
		panel(badge_rect, 13, live ? QColor(254, 44, 85, qMin(255, line + 40)) : soft);
		p.setPen(Qt::NoPen);
		p.setBrush(live ? QColor(254, 44, 85) : QColor(130, 142, 163));
		p.drawEllipse(QPointF(badge_rect.left() + 14, badge_rect.center().y()), 4.5, 4.5);
		text(p, QRectF(badge_rect.left() + 26, badge_rect.top(), badge_rect.width() - 26, badge_rect.height()),
		     Qt::AlignVCenter | Qt::AlignLeft, badge, QColor(238, 242, 249));
		QFont small = font();
		small.setPointSizeF(9);
		p.setFont(small);
		text(p, QRectF(badge_rect.right() + 8, y, W - badge_rect.right() - 8 - kPad, 26), Qt::AlignVCenter | Qt::AlignRight,
		     clock_text(live), QColor(214, 222, 236));
		y += 26 + 8;

		// ---- stat cards
		struct Card { QString label; QString value; int icon; };
		auto number = [&](const QString &key) -> QString {
			const QJsonValue v = stats_.value(key);
			return v.isDouble() ? QLocale().toString(static_cast<qlonglong>(v.toDouble())) : QStringLiteral("–");
		};
		const QJsonValue now = viewers_.isDouble() ? viewers_ : stats_.value(QStringLiteral("viewers"));
		const QJsonValue total = total_.isDouble() ? total_ : stats_.value(QStringLiteral("total"));
		const QString now_text = now.isDouble() ? QLocale().toString(static_cast<qlonglong>(now.toDouble())) : QStringLiteral("–");
		const QString total_text = total.isDouble() ? QLocale().toString(static_cast<qlonglong>(total.toDouble())) : QStringLiteral("–");
		const QVector<Card> cards = {{QStringLiteral("Total"), total_text, 1}, {QStringLiteral("Curtidas"), number(QStringLiteral("likes")), 2},
					     {QStringLiteral("Comentários"), number(QStringLiteral("comments")), 3},
					     {QStringLiteral("Seguidores"), number(QStringLiteral("follows")), 4},
					     {QStringLiteral("Compart."), number(QStringLiteral("shares")), 5},
					     {QStringLiteral("Presentes"), number(QStringLiteral("gifts")), 6}};

		const int inner = W - 2 * kPad;
		// Hero: eye icon + big "now" number on a soft teal-to-pink tint (the layout of the web panel).
		const QRectF hero(kPad, y, inner, 76);
		tinted(hero, 12, edge, 1.0);
		draw_icon(p, 0, QRectF(hero.left() + 16, hero.center().y() - 13, 26, 26), QColor(37, 244, 238));
		QFont big = font();
		big.setPointSizeF(26);
		big.setBold(true);
		p.setFont(big);
		text(p, QRectF(hero.left() + 60, hero.top() + 8, hero.width() - 70, 38), Qt::AlignVCenter | Qt::AlignLeft, now_text,
		     QColor(255, 255, 255));
		p.setFont(small);
		text(p, QRectF(hero.left() + 60, hero.top() + 47, hero.width() - 70, 18), Qt::AlignVCenter | Qt::AlignLeft,
		     QStringLiteral("Agora"), QColor(214, 222, 236));
		y += 76 + 6;

		const int columns = inner < 300 ? 2 : (inner < 440 ? 3 : 6);
		const qreal gap = 6, cw = (inner - gap * (columns - 1)) / columns, ch = 80;
		QFont value_font = font();
		value_font.setPointSizeF(16);
		value_font.setBold(true);
		for (int i = 0; i < cards.size(); ++i) {
			const int col = i % columns, rowi = i / columns;
			const QRectF r(kPad + col * (cw + gap), y + rowi * (ch + gap), cw, ch);
			tinted(r, 12, edge, 0.8);
			draw_icon(p, cards[i].icon, QRectF(r.left() + 13, r.top() + 11, 17, 17), QColor(150, 163, 186));
			p.setFont(value_font);
			text(p, QRectF(r.left() + 13, r.top() + 31, r.width() - 16, 26), Qt::AlignVCenter | Qt::AlignLeft, cards[i].value,
			     QColor(255, 255, 255));
			p.setFont(small);
			text(p, QRectF(r.left() + 13, r.top() + 56, r.width() - 16, 18), Qt::AlignVCenter | Qt::AlignLeft, cards[i].label,
			     QColor(214, 222, 236));
		}
		y += ((cards.size() + columns - 1) / columns) * (ch + gap) + 4;

		// ---- chat
		const QRectF area(kPad, y, inner, H - y - kPad - kSendRoom);
		if (area.height() < 40)
			return;
		// The chat frame has the same fill as the other widgets; every message sits on it in a lighter tone.
		tinted(area, 12, edge, 0.55);
		// Every message uses the same fill as the other widgets; stacked on the chat frame it reads darker.
		paint_feed(p, area.adjusted(8, 6, -8, -6), fill, edge, line);
	}

private:
	static constexpr int kPad = 8;
	static constexpr int kSendRoom = 56;  // bottom strip for the reply box (34 px) plus a one-line status
	static constexpr int kShadow = 12;  // transparent margin around the window that holds the soft shadow
	static constexpr int kEdge = 12;    // resize grab zone (covers the shadow margin)

	// kind: 0 eye, 1 people, 2 heart, 3 chat, 4 user+, 5 share, 6 gift (24x24 grid, 2 px round strokes)
	static void draw_icon(QPainter &p, int kind, const QRectF &r, const QColor &color)
	{
		p.save();
		p.translate(r.topLeft());
		p.scale(r.width() / 24.0, r.height() / 24.0);
		QPen pen(color, 2.0);
		pen.setCapStyle(Qt::RoundCap);
		pen.setJoinStyle(Qt::RoundJoin);
		p.setPen(pen);
		p.setBrush(Qt::NoBrush);
		QPainterPath path;
		switch (kind) {
		case 0:  // eye
			path.moveTo(2, 12);
			path.quadTo(12, -1, 22, 12);
			path.quadTo(12, 25, 2, 12);
			p.drawPath(path);
			p.drawEllipse(QPointF(12, 12), 3.2, 3.2);
			break;
		case 1:  // people
			p.drawEllipse(QPointF(9, 7), 4, 4);
			path.moveTo(2, 21);
			path.lineTo(2, 19);
			path.quadTo(2, 15, 6, 15);
			path.lineTo(12, 15);
			path.quadTo(16, 15, 16, 19);
			path.lineTo(16, 21);
			p.drawPath(path);
			path = QPainterPath();
			path.moveTo(16.5, 3.4);
			path.quadTo(20, 4.5, 19, 8.5);
			path.quadTo(18.4, 10.2, 16.5, 10.8);
			path.moveTo(19, 15.3);
			path.quadTo(22, 16.2, 22, 19);
			path.lineTo(22, 21);
			p.drawPath(path);
			break;
		case 2:  // heart
			path.moveTo(12, 21);
			path.cubicTo(5, 15, 2, 11.5, 2, 8);
			path.cubicTo(2, 5, 4, 3, 7, 3);
			path.cubicTo(9.5, 3, 11, 4.5, 12, 6);
			path.cubicTo(13, 4.5, 14.5, 3, 17, 3);
			path.cubicTo(20, 3, 22, 5, 22, 8);
			path.cubicTo(22, 11.5, 19, 15, 12, 21);
			p.drawPath(path);
			break;
		case 3:  // chat bubble
			path.addRoundedRect(QRectF(3, 3, 18, 14), 3, 3);
			path.moveTo(8, 17);
			path.lineTo(3, 21.5);
			path.lineTo(3, 17);
			p.drawPath(path);
			break;
		case 4:  // user plus
			p.drawEllipse(QPointF(9, 7), 4, 4);
			path.moveTo(2, 21);
			path.lineTo(2, 19);
			path.quadTo(2, 15, 6, 15);
			path.lineTo(12, 15);
			path.quadTo(16, 15, 16, 19);
			path.lineTo(16, 21);
			path.moveTo(19, 8);
			path.lineTo(19, 14);
			path.moveTo(22, 11);
			path.lineTo(16, 11);
			p.drawPath(path);
			break;
		case 5:  // share
			p.drawEllipse(QPointF(18, 5), 3, 3);
			p.drawEllipse(QPointF(6, 12), 3, 3);
			p.drawEllipse(QPointF(18, 19), 3, 3);
			p.drawLine(QPointF(8.6, 13.5), QPointF(15.4, 17.5));
			p.drawLine(QPointF(15.4, 6.5), QPointF(8.6, 10.5));
			break;
		case 6:  // gift
			p.drawRect(QRectF(3, 8, 18, 4));
			path.moveTo(19, 12);
			path.lineTo(19, 21);
			path.lineTo(5, 21);
			path.lineTo(5, 12);
			path.moveTo(12, 8);
			path.lineTo(12, 21);
			p.drawPath(path);
			p.drawEllipse(QPointF(8, 5.2), 2.6, 2.6);
			p.drawEllipse(QPointF(16, 5.2), 2.6, 2.6);
			break;
		}
		p.restore();
	}

	void set_scroll_from_track(qreal y)
	{
		if (!track_.isValid() || thumb_span_ <= 0)
			return;
		const qreal ratio = qBound<qreal>(0, (y - track_.top() - 14) / thumb_span_, 1);  // 14 ~ half a thumb
		scroll_ = static_cast<int>((1.0 - ratio) * max_scroll_);
		update();
	}

	static void text(QPainter &p, const QRectF &r, int flags, const QString &s, const QColor &color)
	{
		p.setPen(QColor(0, 0, 0, 170));  // soft shadow keeps text readable over any background; the text itself is opaque
		p.drawText(r.translated(1, 1), flags, s);
		p.drawText(r.translated(-0.5, 0.5), flags, s);
		p.setPen(color);
		p.drawText(r, flags, s);
	}

	QString clock_text(bool live) const
	{
		if (!live || started_ <= 0)
			return {};
		auto fmt = [](qint64 t) {
			const qint64 h = t / 3600, m = t % 3600 / 60, s = t % 60;
			return h ? QStringLiteral("%1:%2:%3").arg(h).arg(m, 2, 10, QLatin1Char('0')).arg(s, 2, 10, QLatin1Char('0'))
				 : QStringLiteral("%1:%2").arg(m).arg(s, 2, 10, QLatin1Char('0'));
		};
		QString out = QStringLiteral("⏱ ") + fmt(qMax<qint64>(0, QDateTime::currentSecsSinceEpoch() - static_cast<qint64>(started_)));
		if (remaining_ >= 0)
			out += QStringLiteral(" · restam ") + fmt(qMax(0, remaining_ - static_cast<int>((QDateTime::currentMSecsSinceEpoch() - fetched_at_) / 1000)));
		return out;
	}

	struct Row {
		const FeedItem *item = nullptr;
		QString line1, line2;
		qreal t1 = 0, t2 = 0, h = 0, w = 0;  // w: width the bubble needs (the panel's bubbles hug their text too)
		bool joinlike = false, chat = false;
	};

	void paint_feed(QPainter &p, const QRectF &box, const QColor &fill, const QColor &edge, int line)
	{
		Q_UNUSED(line);
		p.save();
		p.setClipRect(box);
		QFont body = font();
		body.setPointSizeF(10);
		QFont name_font = body;
		name_font.setBold(true);
		name_font.setPointSizeF(9);
		const QFontMetricsF bm(body), nm(name_font);
		track_ = QRectF();
		jump_ = QRectF();
		max_scroll_ = 0;

		if (feed_.isEmpty()) {
			p.setFont(body);
			text(p, box, Qt::AlignCenter | Qt::TextWordWrap,
			     phase_ == QLatin1String("live") ? QStringLiteral("Nenhuma mensagem ainda.") : QStringLiteral("Aguardando a LIVE começar…"),
			     QColor(214, 222, 236));
			p.restore();
			return;
		}

		// Pass 1: lay out every message so the scroll range is exact.
		const qreal avatar = 30, gap = 6, bar_room = 10;  // bar_room keeps text clear of the scrollbar
		QVector<Row> rows;
		rows.reserve(feed_.size());
		qreal content = 0;
		for (const FeedItem &item : feed_) {
			Row row;
			row.item = &item;
			row.chat = item.kind == QLatin1String("chat");
			row.joinlike = item.kind == QLatin1String("join");
			const qreal indent = row.joinlike ? 0 : avatar + 8;
			const qreal text_w = box.width() - indent - 20 - bar_room;
			if (row.chat) {
				row.line1 = item.name;
				row.line2 = item.text;
			} else if (row.joinlike) {
				row.line1 = item.name + QStringLiteral(" entrou");
			} else {
				row.line1 = item.name;
				if (item.kind == QLatin1String("gift"))
					row.line2 = QStringLiteral("enviou ") + (item.gift.isEmpty() ? QStringLiteral("um presente") : item.gift) +
						    (item.count > 1 ? QStringLiteral(" × %1").arg(item.count) : QString()) +
						    (item.coins ? QStringLiteral(" · %1 moedas").arg(item.coins) : QString());
				else if (item.kind == QLatin1String("follow"))
					row.line2 = QStringLiteral("começou a seguir você");
				else if (item.kind == QLatin1String("share"))
					row.line2 = QStringLiteral("compartilhou a LIVE");
				else
					row.line2 = QStringLiteral("entrou no clube de fãs") + (item.rank.isEmpty() ? QString() : QStringLiteral(" (nº %1)").arg(item.rank));
			}
			row.t1 = row.joinlike ? bm.height() : nm.height();
			const QRectF wrapped = row.line2.isEmpty() ? QRectF() : bm.boundingRect(QRectF(0, 0, text_w, 10000), Qt::TextWordWrap, row.line2);
			row.t2 = wrapped.height();
			row.w = qMax(nm.horizontalAdvance(row.line1), wrapped.width()) + 24;
			row.h = qMax(row.joinlike ? bm.height() + 8 : avatar + 4, 10 + row.t1 + row.t2);
			content += row.h + (rows.isEmpty() ? 0 : gap);
			rows.append(row);
		}
		max_scroll_ = qMax<qreal>(0, content - box.height());
		// Keep the reader's place when new messages arrive while scrolled up.
		if (scroll_ > 0 && last_content_ > 0 && content > last_content_)
			scroll_ += static_cast<int>(content - last_content_);
		last_content_ = content;
		scroll_ = qBound(0, scroll_, static_cast<int>(max_scroll_));

		// Pass 2: draw from the newest message upwards, lifted by `scroll_`.
		qreal bottom = box.bottom() + scroll_;
		for (int i = rows.size() - 1; i >= 0; --i) {
			const Row &row = rows[i];
			const FeedItem &item = *row.item;
			const qreal top = bottom - row.h;
			if (bottom < box.top())
				break;
			if (top <= box.bottom()) {
				if (row.joinlike) {
					p.setFont(body);
					const qreal w = bm.horizontalAdvance(row.line1) + 18;
					const QRectF pill(box.left() + (box.width() - bar_room - w) / 2, top + 1, w, row.h - 2);
					p.setPen(Qt::NoPen);
					p.setBrush(fill);
					p.drawRoundedRect(pill, pill.height() / 2, pill.height() / 2);
					text(p, pill, Qt::AlignCenter, row.line1, QColor(214, 222, 236));
				} else {
					draw_avatar(p, QRectF(box.left() + 2, top + 2, avatar, avatar), item);
					const qreal max_w = box.width() - avatar - 10 - bar_room;
					const QRectF bubble(box.left() + avatar + 8, top, qBound<qreal>(60, row.w, max_w), row.h - 2);
					// Same shape as the panel's bubble: small corner at the top-left (the avatar side), thin border.
					const qreal big = 13, small_r = 4;
					QPainterPath shape;
					shape.moveTo(bubble.left() + small_r, bubble.top());
					shape.lineTo(bubble.right() - big, bubble.top());
					shape.quadTo(bubble.topRight(), QPointF(bubble.right(), bubble.top() + big));
					shape.lineTo(bubble.right(), bubble.bottom() - big);
					shape.quadTo(bubble.bottomRight(), QPointF(bubble.right() - big, bubble.bottom()));
					shape.lineTo(bubble.left() + big, bubble.bottom());
					shape.quadTo(bubble.bottomLeft(), QPointF(bubble.left(), bubble.bottom() - big));
					shape.lineTo(bubble.left(), bubble.top() + small_r);
					shape.quadTo(bubble.topLeft(), QPointF(bubble.left() + small_r, bubble.top()));
					p.setPen(QPen(edge, 1));
					p.setBrush(fill);
					p.drawPath(shape);
					p.setFont(name_font);
					text(p, QRectF(bubble.left() + 10, top + 4, bubble.width() - 20, row.t1 + 2), Qt::AlignLeft | Qt::AlignTop, row.line1,
					     QColor::fromHsl(name_hue(item.name), 200, 190));
					if (!row.line2.isEmpty()) {
						p.setFont(body);
						text(p, QRectF(bubble.left() + 10, top + 4 + row.t1 + 1, bubble.width() - 20, row.t2 + 2),
						     Qt::AlignLeft | Qt::AlignTop | Qt::TextWordWrap, row.line2, QColor(255, 255, 255));
					}
				}
			}
			bottom = top - gap;
		}

		// Scrollbar (only when there is something to scroll) and the "go to the end" pill.
		if (max_scroll_ > 0) {
			track_ = QRectF(box.right() - 6, box.top() + 2, 4, box.height() - 4);
			p.setPen(Qt::NoPen);
			p.setBrush(QColor(255, 255, 255, 28));
			p.drawRoundedRect(track_, 2, 2);
			const qreal thumb_h = qMax<qreal>(28, track_.height() * box.height() / content);
			thumb_span_ = track_.height() - thumb_h;
			const qreal thumb_top = track_.top() + thumb_span_ * (1.0 - scroll_ / max_scroll_);
			p.setBrush(QColor(255, 255, 255, dragging_ ? 190 : 120));
			p.drawRoundedRect(QRectF(track_.left(), thumb_top, track_.width(), thumb_h), 2, 2);
		}
		if (scroll_ > 0) {
			p.setFont(body);
			const QString label = QStringLiteral("↓ Ir ao fim");
			const qreal w = bm.horizontalAdvance(label) + 24;
			jump_ = QRectF(box.center().x() - w / 2, box.bottom() - 30, w, 24);
			p.setPen(Qt::NoPen);
			p.setBrush(QColor(37, 244, 238));
			p.drawRoundedRect(jump_, 12, 12);
			p.setPen(QColor(7, 34, 42));
			p.drawText(jump_, Qt::AlignCenter, label);
		}
		p.restore();
	}

	static int name_hue(const QString &name)
	{
		uint h = 0;
		for (const QChar c : name)
			h = (h * 31 + c.unicode()) % 360;
		return static_cast<int>(h);
	}

	void draw_avatar(QPainter &p, const QRectF &r, const FeedItem &item)
	{
		p.save();
		QPainterPath clip;
		clip.addEllipse(r);
		p.setClipPath(clip, Qt::IntersectClip);
		auto it = photos_.constFind(item.avatar);
		if (!item.avatar.isEmpty() && it != photos_.constEnd() && !it->isNull()) {
			p.drawPixmap(r.toRect(), *it);  // photo: always opaque
		} else {
			p.fillRect(r, QColor::fromHsl(name_hue(item.name), 170, 150));
			QFont f = font();
			f.setBold(true);
			p.setFont(f);
			p.setPen(QColor(12, 16, 24));
			p.drawText(r, Qt::AlignCenter, item.name.left(1).toUpper());
			if (!item.avatar.isEmpty())
				request_photo(item.avatar);
		}
		p.restore();
	}

	void request_photo(const QString &url)
	{
		if (photos_.contains(url) || pending_.contains(url) || !url.startsWith(QStringLiteral("https://")))
			return;
		const QString host = QUrl(url).host();
		if (!(host.endsWith(QStringLiteral(".tiktokcdn.com")) || host.endsWith(QStringLiteral(".tiktokcdn-us.com")) ||
		      host.endsWith(QStringLiteral(".tiktokcdn-eu.com"))))
			return;  // only TikTok's own image CDNs
		pending_.insert(url);
		// Qt inside OBS has no HTTPS backend: ask the controller (loopback) to fetch the photo.
		QNetworkReply *reply = net_->get(QNetworkRequest(QUrl(base() + QStringLiteral("/avatar?u=") + QString::fromUtf8(QUrl::toPercentEncoding(url)))));
		QObject::connect(reply, &QNetworkReply::finished, this, [this, reply, url] {
			QPixmap pixmap;
			if (reply->error() == QNetworkReply::NoError)
				pixmap.loadFromData(reply->readAll());
			photos_.insert(url, pixmap.isNull() ? pixmap : pixmap.scaled(64, 64, Qt::KeepAspectRatioByExpanding, Qt::SmoothTransformation));
			pending_.remove(url);
			reply->deleteLater();
			update();
		});
	}

	void set_send_enabled(bool on)
	{
		input_->setEnabled(on && !sending_);
		send_btn_->setEnabled(on && !sending_);
		input_->setPlaceholderText(on ? QStringLiteral("Responder no chat da LIVE…") : QStringLiteral("Disponível durante a LIVE"));
	}

	void show_send_status(const QString &text, bool bad)
	{
		send_status_->setText(text);
		send_status_->setStyleSheet(QStringLiteral("QLabel{font-size:11px;font-weight:500;color:%1}").arg(bad ? QStringLiteral("#ff8aa0") : QStringLiteral("#8d9bb3")));
		QTimer::singleShot(6000, this, [this, text] {
			if (send_status_->text() == text)
				send_status_->clear();
		});
	}

	void send_message()
	{
		const QString text = input_->text().trimmed();
		if (text.isEmpty() || sending_)
			return;
		sending_ = true;
		set_send_enabled(phase_ == QLatin1String("live"));
		if (token_.isEmpty()) {
			QNetworkReply *reply = net_->get(QNetworkRequest(QUrl(base() + QStringLiteral("/api/session"))));
			QObject::connect(reply, &QNetworkReply::finished, this, [this, reply, text] {
				token_ = QJsonDocument::fromJson(reply->readAll()).object().value(QStringLiteral("token")).toString();
				reply->deleteLater();
				if (token_.isEmpty()) {
					finish_send(false, QStringLiteral("Controlador indisponível."));
					return;
				}
				post_message(text);
			});
			return;
		}
		post_message(text);
	}

	void post_message(const QString &text)
	{
		QNetworkRequest request(QUrl(base() + QStringLiteral("/api/chat")));
		request.setHeader(QNetworkRequest::ContentTypeHeader, QStringLiteral("application/json"));
		request.setRawHeader("Origin", base().toUtf8());
		request.setRawHeader("X-Panel-Token", token_.toUtf8());
		QNetworkReply *reply = net_->post(request, QJsonDocument(QJsonObject{{QStringLiteral("text"), text}}).toJson(QJsonDocument::Compact));
		QObject::connect(reply, &QNetworkReply::finished, this, [this, reply] {
			const int status = reply->attribute(QNetworkRequest::HttpStatusCodeAttribute).toInt();
			const QJsonObject body = QJsonDocument::fromJson(reply->readAll()).object();
			reply->deleteLater();
			if (status == 403)
				token_.clear();  // the controller restarted: fetch a new token on the next try
			if (status >= 200 && status < 300) {
				input_->clear();
				finish_send(true, QStringLiteral("Enviada."));
			} else {
				const QString error = body.value(QStringLiteral("error")).toString();
				finish_send(false, error.isEmpty() ? QStringLiteral("Não foi possível enviar.") : error);
			}
		});
	}

	void finish_send(bool ok, const QString &message)
	{
		sending_ = false;
		set_send_enabled(phase_ == QLatin1String("live"));
		show_send_status(message, !ok);
		if (input_->isEnabled())
			input_->setFocus();
	}

	void fetch_state()
	{
		QNetworkReply *reply = net_->get(QNetworkRequest(QUrl(base() + QStringLiteral("/api/state"))));
		QObject::connect(reply, &QNetworkReply::finished, this, [this, reply] {
			const QByteArray body = reply->readAll();
			const bool ok = reply->error() == QNetworkReply::NoError;
			reply->deleteLater();
			if (!ok)
				return;
			const QJsonObject state = QJsonDocument::fromJson(body).object();
			phase_ = state.value(QStringLiteral("phase")).toString();
				set_send_enabled(phase_ == QLatin1String("live"));
			started_ = state.value(QStringLiteral("startedAt")).toDouble(0);
			remaining_ = state.value(QStringLiteral("remainingSec")).isDouble() ? state.value(QStringLiteral("remainingSec")).toInt() : -1;
			fetched_at_ = QDateTime::currentMSecsSinceEpoch();
			const QJsonObject view = state.value(QStringLiteral("view")).toObject();
			stats_ = view.value(QStringLiteral("stats")).toObject();
			viewers_ = view.value(QStringLiteral("viewers"));
			total_ = view.value(QStringLiteral("total"));
			feed_.clear();
			for (const QJsonValue &value : view.value(QStringLiteral("feed")).toArray()) {
				const QJsonObject o = value.toObject();
				FeedItem item;
				item.n = static_cast<qint64>(o.value(QStringLiteral("n")).toDouble());
				item.kind = o.value(QStringLiteral("k")).toString();
				item.name = o.value(QStringLiteral("name")).toString().left(80);
				item.text = o.value(QStringLiteral("text")).toString().left(300);
				item.gift = o.value(QStringLiteral("gift")).toString().left(60);
				item.rank = o.value(QStringLiteral("rank")).toString().left(10);
				item.avatar = o.value(QStringLiteral("avatar")).toString();
				item.count = o.value(QStringLiteral("count")).toInt();
				item.coins = o.value(QStringLiteral("coins")).toInt();
				feed_.append(item);
			}
			update();
		});
	}

	Qt::Edges edges_at(const QPoint &pt) const
	{
		Qt::Edges edges;
		if (pt.x() < kEdge)
			edges |= Qt::LeftEdge;
		if (pt.x() >= width() - kEdge)
			edges |= Qt::RightEdge;
		if (pt.y() < kEdge)
			edges |= Qt::TopEdge;
		if (pt.y() >= height() - kEdge)
			edges |= Qt::BottomEdge;
		return edges;
	}

	static Qt::CursorShape cursor_for(Qt::Edges e)
	{
		if ((e & Qt::LeftEdge && e & Qt::TopEdge) || (e & Qt::RightEdge && e & Qt::BottomEdge))
			return Qt::SizeFDiagCursor;
		if ((e & Qt::RightEdge && e & Qt::TopEdge) || (e & Qt::LeftEdge && e & Qt::BottomEdge))
			return Qt::SizeBDiagCursor;
		if (e & (Qt::LeftEdge | Qt::RightEdge))
			return Qt::SizeHorCursor;
		if (e & (Qt::TopEdge | Qt::BottomEdge))
			return Qt::SizeVerCursor;
		return Qt::ArrowCursor;
	}

	QWidget *bar_ = nullptr;
	QSlider *slider_ = nullptr;
	QToolButton *pin_ = nullptr;
	QNetworkAccessManager *net_ = nullptr;
	QWidget *send_ = nullptr;
	QLineEdit *input_ = nullptr;
	QToolButton *send_btn_ = nullptr;
	QLabel *send_status_ = nullptr;
	QString token_;
	bool sending_ = false;
	int bg_ = 45;
	int scroll_ = 0;        // pixels scrolled up from the newest message (0 = following the chat)
	qreal max_scroll_ = 0, last_content_ = 0, thumb_span_ = 0;
	bool dragging_ = false;
	QRectF track_, jump_;   // in body coordinates (window minus the shadow margin)
	QString phase_;
	double started_ = 0;
	int remaining_ = -1;
	qint64 fetched_at_ = 0;
	QJsonObject stats_;
	QJsonValue viewers_, total_;
	QVector<FeedItem> feed_;
	QHash<QString, QPixmap> photos_;
	QSet<QString> pending_;
};

void open_floating_feed(void *)
{
	if (g_floating) {
		g_floating->raise();
		g_floating->activateWindow();
		return;
	}
	g_floating = new FloatingOverlay();
	g_floating->show();
}

// First run only: dock the panel on the right so it is visible, then leave the layout to the user.
void place_docks_first_time()
{
	QJsonObject config = read_config();
	// Layout 3 = the single "LiveTT" panel, placed after OBS restored its saved layout. A profile that was laid out for the old two-panel version is placed again once.
	if (config.value(QStringLiteral("docks_layout")).toInt() >= 3)
		return;
	auto *main = static_cast<QMainWindow *>(obs_frontend_get_main_window());
	if (!main)
		return;
	QDockWidget *control = main->findChild<QDockWidget *>(kControlId);
	if (control) {
		control->setFloating(false);
		main->addDockWidget(Qt::RightDockWidgetArea, control);
		control->show();
		main->resizeDocks({control}, {400}, Qt::Horizontal); // sensible first width; the user can resize freely
	}
	blog(LOG_INFO, "[livett] docks placed (control=%d)", control != nullptr);
	config.insert(QStringLiteral("docks_layout"), 3);
	write_config(config);
}

// Put the dock back in the default spot: docked on the right, comfortable width.
void redock(QPointer<QDockWidget> dock)
{
	auto *main = static_cast<QMainWindow *>(obs_frontend_get_main_window());
	if (!main || !dock)
		return;
	dock->setFloating(false);
	main->addDockWidget(Qt::RightDockWidgetArea, dock);
	dock->show();
	dock->raise();
	main->resizeDocks({dock}, {400}, Qt::Horizontal);
	blog(LOG_INFO, "[livett] redock %s (floating=%d)", dock->objectName().toUtf8().constData(),
	     dock->isFloating());
}

// Clicking the item in the Docks menu (a user action, not OBS restoring its saved layout) re-docks the panel.
void hook_menu_redock()
{
	auto *main = static_cast<QMainWindow *>(obs_frontend_get_main_window());
	if (!main)
		return;
	QPointer<QDockWidget> control = main->findChild<QDockWidget *>(kControlId);
	if (!control)
		return;
	QObject::connect(control->toggleViewAction(), &QAction::triggered, control, [control](bool checked) {
		if (checked)
			QTimer::singleShot(0, control, [control] { redock(control); });
	});
}

void add_docks()
{
	if (g_docks_added)
		return;
	g_docks_added = true;
	// One panel: the LIVE's status and live view, plus the extra settings. (Starting is done in the start window.)
	if (QWidget *control = make_web_widget(QStringLiteral("/")))
		obs_frontend_add_dock_by_id(kControlId, "LiveTT", control);
	blog(LOG_INFO, "[livett] docks added");
	// OBS restores its saved dock layout after the plugins load, which would undo the placement: do it afterwards.
	QTimer::singleShot(2500, [] { place_docks_first_time(); });
	hook_menu_redock();
}

// Runs on the UI thread once OBS finished loading.
// The controller is a separate process; if it dies (crash, killed) the docks would show "connection refused" forever.
// Relaunch it after two failed checks in a row (~10 s), at most once per 30 s. A second instance exits at once when the
// port is taken, and a controller that died mid-LIVE recovers from its journal.
void supervise_controller()
{
	static int misses = 0;
	static qint64 last_launch_ms = 0;
	auto *timer = new QTimer();
	QObject::connect(timer, &QTimer::timeout, [] {
		if (controller_listening()) {
			misses = 0;
			return;
		}
		const qint64 now = QDateTime::currentMSecsSinceEpoch();
		if (++misses < 2 || now - last_launch_ms < 30000)
			return;
		last_launch_ms = now;
		blog(LOG_WARNING, "[livett] controller not answering; relaunching");
		start_controller();
	});
	timer->start(5000);
}

void on_loaded()
{
	// The controller reads OBS through this API (replaces obs-websocket): it must be up before the controller starts.
	if (!obs_api_start())
		blog(LOG_WARNING, "[livett] control API did not start; the controller will not reach OBS");
	tiktok_service_watch_start_button();
	supervise_controller();
	if (read_config().value(QStringLiteral("open_floating_on_start")).toBool())
		QTimer::singleShot(1500, [] { open_floating_feed(nullptr); });
	if (controller_listening()) {
		add_docks();
		return;
	}
	start_controller();
	// Add the docks only when the page can actually load: poll for up to ~20 s without blocking the UI.
	g_wait = new QTimer();
	QObject::connect(g_wait, &QTimer::timeout, [] {
		++g_wait_ticks;
		if (controller_listening() || g_wait_ticks > 40) {
			g_wait->stop();
			g_wait->deleteLater();
			g_wait = nullptr;
			add_docks();  // after the timeout the dock shows OBS's own "retry" page if the controller is down
		}
	});
	g_wait->start(500);
}

void on_event(enum obs_frontend_event event, void *)
{
	tiktok_service_on_frontend_event(event);
	if (event == OBS_FRONTEND_EVENT_FINISHED_LOADING)
		on_loaded();
}

} // namespace

void tiktok_show_panel()
{
	QMainWindow *main = static_cast<QMainWindow *>(obs_frontend_get_main_window());
	QDockWidget *dock = main ? main->findChild<QDockWidget *>(kControlId) : nullptr;
	if (!dock)
		return;
	// A floating or hidden dock is put back in the default spot (a floating browser widget comes back blank).
	QTimer::singleShot(0, dock, [dock] { redock(dock); });
}

bool obs_module_load(void)
{
	blog(LOG_INFO, "[livett] loaded (0.2.0)");
	tiktok_service_register(); // "LiveTT" service: the OBS start button can begin a LIVE (see obs-service.cpp)
	tiktok_phone_camera_register(); // source "Câmera do celular": its QR code opens when it is added (see phone-camera.cpp)
	obs_frontend_add_event_callback(on_event, nullptr);
	obs_frontend_add_tools_menu_item("LiveTT: janela flutuante Ao vivo", open_floating_feed, nullptr);
	return true;
}

void obs_module_unload(void)
{
	obs_frontend_remove_event_callback(on_event, nullptr);
	obs_api_stop();
	if (g_floating)
		g_floating->close();
	if (g_docks_added)
		obs_frontend_remove_dock(kControlId);
}
