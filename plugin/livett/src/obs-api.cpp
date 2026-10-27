// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

#include "obs-api.hpp"
#include "obs-service.hpp"

#include <obs-module.h>
#include <obs-frontend-api.h>
#include <obs.hpp>
#include <util/platform.h>
#include <graphics/graphics.h>
#include <graphics/vec4.h>

#include <windows.h>

#include <QBuffer>
#include <QByteArray>
#include <QDateTime>
#include <QDir>
#include <QFile>
#include <QHash>
#include <QImage>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QPointer>
#include <QRandomGenerator>
#include <QTcpServer>
#include <QTcpSocket>
#include <QTimer>

#include <functional>

namespace {

using Json = QJsonObject;

struct Reply {
	bool ok = true;
	Json data;
	QString error;
	int code = 600;
};

Reply ok(Json data = {})
{
	Reply r;
	r.data = std::move(data);
	return r;
}

Reply fail(const QString &message, int code = 600)
{
	Reply r;
	r.ok = false;
	r.error = message;
	r.code = code;
	return r;
}

QString text(const Json &o, const char *key) { return o.value(QLatin1String(key)).toString(); }

QJsonObject dataToJson(obs_data_t *data)
{
	if (!data)
		return {};
	const char *raw = obs_data_get_json(data);
	return QJsonDocument::fromJson(QByteArray(raw ? raw : "{}")).object();
}

// Caller releases the result.
obs_data_t *jsonToData(const Json &o)
{
	const QByteArray raw = QJsonDocument(o).toJson(QJsonDocument::Compact);
	obs_data_t *data = obs_data_create_from_json(raw.constData());
	return data ? data : obs_data_create();
}

// ---- handlers (all run on the UI thread)

Reply getVersion(const Json &)
{
	return ok({{"obsVersion", QString::fromUtf8(obs_get_version_string())},
		   {"obsWebSocketVersion", "livett-api/1"},
		   {"rpcVersion", 1}});
}

Reply getProfileList(const Json &)
{
	char *current = obs_frontend_get_current_profile();
	QJsonArray names;
	if (char **list = obs_frontend_get_profiles()) {
		for (char **p = list; *p; ++p)
			names.append(QString::fromUtf8(*p));
		bfree(list);
	}
	const QString name = QString::fromUtf8(current ? current : "");
	bfree(current);
	return ok({{"currentProfileName", name}, {"profiles", names}});
}

Reply getCurrentProgramScene(const Json &)
{
	OBSSourceAutoRelease scene = obs_frontend_get_current_scene();
	const QString name = scene ? QString::fromUtf8(obs_source_get_name(scene)) : QString();
	return ok({{"currentProgramSceneName", name}, {"sceneName", name}});
}

Reply getSceneList(const Json &)
{
	struct obs_frontend_source_list list = {};
	obs_frontend_get_scenes(&list);
	QJsonArray scenes;
	for (size_t i = 0; i < list.sources.num; i++)
		scenes.append(Json{{"sceneName", QString::fromUtf8(obs_source_get_name(list.sources.array[i]))}, {"sceneIndex", (int)i}});
	obs_frontend_source_list_free(&list);
	OBSSourceAutoRelease current = obs_frontend_get_current_scene();
	return ok({{"currentProgramSceneName", current ? QString::fromUtf8(obs_source_get_name(current)) : QString()}, {"scenes", scenes}});
}

Reply setCurrentProgramScene(const Json &d)
{
	const QString name = text(d, "sceneName");
	OBSSourceAutoRelease source = obs_get_source_by_name(name.toUtf8().constData());
	if (!source || !obs_source_is_scene(source))
		return fail(QStringLiteral("No scene named that"), 601);
	obs_frontend_set_current_scene(source);
	return ok();
}

Reply getStreamStatus(const Json &)
{
	Json out{{"outputActive", obs_frontend_streaming_active()}, {"outputReconnecting", false}, {"pluginStarting", tiktok_service_start_in_progress()}};
	OBSOutputAutoRelease output = obs_frontend_get_streaming_output();
	if (output) {
		out["outputReconnecting"] = obs_output_reconnecting(output);
		out["outputBytes"] = (double)obs_output_get_total_bytes(output);
		out["outputSkippedFrames"] = obs_output_get_frames_dropped(output);
		out["outputTotalFrames"] = obs_output_get_total_frames(output);
		out["outputCongestion"] = obs_output_get_congestion(output);
	}
	return ok(out);
}

Reply getStats(const Json &)
{
	static os_cpu_usage_info_t *cpu = os_cpu_usage_info_start();
	Json out;
	out["cpuUsage"] = cpu ? os_cpu_usage_info_query(cpu) : 0.0;
	out["memoryUsage"] = (double)os_get_proc_resident_size() / (1024.0 * 1024.0);
	char *profile = obs_frontend_get_current_profile_path();
	out["availableDiskSpace"] = profile ? (double)os_get_free_disk_space(profile) / (1024.0 * 1024.0) : 0.0;
	bfree(profile);
	out["activeFps"] = obs_get_active_fps();
	out["averageFrameRenderTime"] = (double)obs_get_average_frame_time_ns() / 1000000.0;
	out["renderSkippedFrames"] = (int)obs_get_lagged_frames();
	out["renderTotalFrames"] = (int)obs_get_total_frames();
	OBSOutputAutoRelease output = obs_frontend_get_streaming_output();
	out["outputSkippedFrames"] = output ? (int)obs_output_get_frames_dropped(output) : 0;
	out["outputTotalFrames"] = output ? (int)obs_output_get_total_frames(output) : 0;
	return ok(out);
}

Reply getVideoSettings(const Json &)
{
	struct obs_video_info ovi = {};
	if (!obs_get_video_info(&ovi))
		return fail(QStringLiteral("Video is not initialized"));
	return ok({{"baseWidth", (int)ovi.base_width},
		   {"baseHeight", (int)ovi.base_height},
		   {"outputWidth", (int)ovi.output_width},
		   {"outputHeight", (int)ovi.output_height},
		   {"fpsNumerator", (int)ovi.fps_num},
		   {"fpsDenominator", (int)ovi.fps_den}});
}

Reply getStreamServiceSettings(const Json &)
{
	obs_service_t *service = obs_frontend_get_streaming_service();
	if (!service)
		return fail(QStringLiteral("No streaming service"));
	OBSDataAutoRelease settings = obs_service_get_settings(service);
	return ok({{"streamServiceType", QString::fromUtf8(obs_service_get_type(service))}, {"streamServiceSettings", dataToJson(settings)}});
}

bool serviceTypeExists(const QString &type)
{
	const char *id = nullptr;
	for (size_t i = 0; obs_enum_service_types(i, &id); i++)
		if (id && type == QString::fromUtf8(id))
			return true;
	return false;
}

Reply setStreamServiceSettings(const Json &d)
{
	const QString type = text(d, "streamServiceType");
	if (!serviceTypeExists(type))
		return fail(QStringLiteral("Unknown service type"), 602);
	OBSDataAutoRelease settings = jsonToData(d.value(QLatin1String("streamServiceSettings")).toObject());
	obs_service_t *current = obs_frontend_get_streaming_service();
	if (current && type == QString::fromUtf8(obs_service_get_type(current))) {
		// Same type: merge into what is there (like obs-websocket), so unrelated settings survive.
		OBSDataAutoRelease merged = obs_service_get_settings(current);
		OBSDataAutoRelease copy = obs_data_create();
		obs_data_apply(copy, merged);
		obs_data_apply(copy, settings);
		obs_service_update(current, copy);
	} else {
		OBSServiceAutoRelease created = obs_service_create(type.toUtf8().constData(), "default_service", settings, nullptr);
		if (!created)
			return fail(QStringLiteral("Could not create the service"));
		obs_frontend_set_streaming_service(created);
	}
	obs_frontend_save_streaming_service();
	return ok();
}

Reply startStream(const Json &)
{
	if (obs_frontend_streaming_active())
		return fail(QStringLiteral("The stream is already active"), 500);
	obs_frontend_streaming_start();
	return ok();
}

Reply stopStream(const Json &)
{
	if (!obs_frontend_streaming_active()) {
		OBSOutputAutoRelease output = obs_frontend_get_streaming_output();
		if (!output || !obs_output_reconnecting(output))
			return fail(QStringLiteral("The stream is not active"), 501);
	}
	obs_frontend_streaming_stop();
	return ok();
}

struct InputCollector {
	QString kind;
	QJsonArray inputs;
};

bool collectInput(void *param, obs_source_t *source)
{
	auto *c = static_cast<InputCollector *>(param);
	if (obs_source_get_type(source) != OBS_SOURCE_TYPE_INPUT)
		return true;
	const QString id = QString::fromUtf8(obs_source_get_id(source));
	if (!c->kind.isEmpty() && id != c->kind)
		return true;
	c->inputs.append(Json{{"inputName", QString::fromUtf8(obs_source_get_name(source))},
			      {"inputKind", id},
			      {"unversionedInputKind", QString::fromUtf8(obs_source_get_unversioned_id(source))}});
	return true;
}

Reply getInputList(const Json &d)
{
	InputCollector c{text(d, "inputKind"), {}};
	obs_enum_sources(collectInput, &c);
	return ok({{"inputs", c.inputs}});
}

obs_source_t *findInput(const Json &d, Reply &error)
{
	const QString name = text(d, "inputName");
	obs_source_t *source = obs_get_source_by_name(name.toUtf8().constData());
	if (!source || obs_source_get_type(source) != OBS_SOURCE_TYPE_INPUT) {
		obs_source_release(source);
		error = fail(QStringLiteral("No input named that"), 601);
		return nullptr;
	}
	return source;
}

Reply getInputSettings(const Json &d)
{
	Reply error;
	OBSSourceAutoRelease source = findInput(d, error);
	if (!source)
		return error;
	OBSDataAutoRelease settings = obs_source_get_settings(source);
	return ok({{"inputSettings", dataToJson(settings)}, {"inputKind", QString::fromUtf8(obs_source_get_id(source))}});
}

Reply setInputSettings(const Json &d)
{
	Reply error;
	OBSSourceAutoRelease source = findInput(d, error);
	if (!source)
		return error;
	OBSDataAutoRelease settings = jsonToData(d.value(QLatin1String("inputSettings")).toObject());
	if (d.contains(QLatin1String("overlay")) && !d.value(QLatin1String("overlay")).toBool(true))
		obs_source_reset_settings(source, settings);
	else
		obs_source_update(source, settings);
	return ok();
}

Reply getInputMute(const Json &d)
{
	Reply error;
	OBSSourceAutoRelease source = findInput(d, error);
	if (!source)
		return error;
	return ok({{"inputMuted", obs_source_muted(source)}});
}

Reply getInputAudioTracks(const Json &d)
{
	Reply error;
	OBSSourceAutoRelease source = findInput(d, error);
	if (!source)
		return error;
	const uint32_t mixers = obs_source_get_audio_mixers(source);
	Json tracks;
	for (int i = 0; i < 6; i++)
		tracks[QString::number(i + 1)] = (mixers & (1u << i)) != 0;
	return ok({{"inputAudioTracks", tracks}});
}

Reply triggerMediaInputAction(const Json &d)
{
	Reply error;
	OBSSourceAutoRelease source = findInput(d, error);
	if (!source)
		return error;
	const QString action = text(d, "mediaAction");
	if (action == QLatin1String("OBS_WEBSOCKET_MEDIA_INPUT_ACTION_PLAY"))
		obs_source_media_play_pause(source, false);
	else if (action == QLatin1String("OBS_WEBSOCKET_MEDIA_INPUT_ACTION_PAUSE"))
		obs_source_media_play_pause(source, true);
	else if (action == QLatin1String("OBS_WEBSOCKET_MEDIA_INPUT_ACTION_STOP"))
		obs_source_media_stop(source);
	else if (action == QLatin1String("OBS_WEBSOCKET_MEDIA_INPUT_ACTION_RESTART"))
		obs_source_media_restart(source);
	else if (action == QLatin1String("OBS_WEBSOCKET_MEDIA_INPUT_ACTION_NEXT"))
		obs_source_media_next(source);
	else if (action == QLatin1String("OBS_WEBSOCKET_MEDIA_INPUT_ACTION_PREVIOUS"))
		obs_source_media_previous(source);
	else
		return fail(QStringLiteral("Unknown media action"), 602);
	return ok();
}

Reply createScene(const Json &d)
{
	const QByteArray name = text(d, "sceneName").toUtf8();
	if (name.isEmpty())
		return fail(QStringLiteral("sceneName is required"), 602);
	OBSSourceAutoRelease existing = obs_get_source_by_name(name.constData());
	if (existing)
		return fail(QStringLiteral("A source with that name already exists"), 601);
	obs_scene_t *scene = obs_scene_create(name.constData());
	if (!scene)
		return fail(QStringLiteral("Could not create the scene"));
	obs_scene_release(scene);
	return ok();
}

Reply createInput(const Json &d)
{
	const QByteArray sceneName = text(d, "sceneName").toUtf8();
	const QByteArray inputName = text(d, "inputName").toUtf8();
	const QByteArray kind = text(d, "inputKind").toUtf8();
	if (inputName.isEmpty() || kind.isEmpty())
		return fail(QStringLiteral("inputName and inputKind are required"), 602);
	OBSSourceAutoRelease sceneSource = obs_get_source_by_name(sceneName.constData());
	obs_scene_t *scene = sceneSource ? obs_scene_from_source(sceneSource) : nullptr;
	if (!scene)
		return fail(QStringLiteral("No scene named that"), 601);
	OBSSourceAutoRelease existing = obs_get_source_by_name(inputName.constData());
	if (existing)
		return fail(QStringLiteral("A source with that name already exists"), 601);
	bool kindKnown = false;
	const char *id = nullptr;
	for (size_t i = 0; obs_enum_input_types2(i, &id, nullptr); i++)
		if (id && kind == id)
			kindKnown = true;
	if (!kindKnown)
		return fail(QStringLiteral("Unknown input kind"), 602);
	OBSDataAutoRelease settings = jsonToData(d.value(QLatin1String("inputSettings")).toObject());
	OBSSourceAutoRelease input = obs_source_create(kind.constData(), inputName.constData(), settings, nullptr);
	if (!input)
		return fail(QStringLiteral("Could not create the input"));
	obs_sceneitem_t *item = obs_scene_add(scene, input);
	if (!item)
		return fail(QStringLiteral("Could not add the input to the scene"));
	obs_sceneitem_set_visible(item, d.value(QLatin1String("sceneItemEnabled")).toBool(true));
	return ok({{"sceneItemId", (int)obs_sceneitem_get_id(item)}, {"inputUuid", QString::fromUtf8(obs_source_get_uuid(input))}});
}

QImage renderSource(obs_source_t *source, uint32_t width, uint32_t height)
{
	const uint32_t sw = obs_source_get_base_width(source), sh = obs_source_get_base_height(source);
	if (sw == 0 || sh == 0 || width == 0 || height == 0)
		return {};
	QImage out;
	obs_enter_graphics();
	gs_texrender_t *tex = gs_texrender_create(GS_RGBA, GS_ZS_NONE);
	gs_stagesurf_t *stage = gs_stagesurface_create(width, height, GS_RGBA);
	gs_texrender_reset(tex);
	if (gs_texrender_begin(tex, width, height)) {
		vec4 background;
		vec4_zero(&background);
		gs_clear(GS_CLEAR_COLOR, &background, 0.0f, 0);
		gs_ortho(0.0f, (float)sw, 0.0f, (float)sh, -100.0f, 100.0f);
		gs_blend_state_push();
		gs_blend_function(GS_BLEND_ONE, GS_BLEND_ZERO);
		obs_source_inc_showing(source);
		obs_source_video_render(source);
		obs_source_dec_showing(source);
		gs_blend_state_pop();
		gs_texrender_end(tex);
		gs_stage_texture(stage, gs_texrender_get_texture(tex));
		uint8_t *data = nullptr;
		uint32_t linesize = 0;
		if (gs_stagesurface_map(stage, &data, &linesize)) {
			out = QImage(data, (int)width, (int)height, (qsizetype)linesize, QImage::Format_RGBA8888).copy();
			gs_stagesurface_unmap(stage);
		}
	}
	gs_stagesurface_destroy(stage);
	gs_texrender_destroy(tex);
	obs_leave_graphics();
	return out;
}

// Opens the plugin's start window (what a click on OBS's start button opens). It only asks; nothing is created until the user confirms.
Reply openStartDialog(const Json &)
{
	tiktok_service_request_start();
	return ok();
}

Reply getSourceScreenshot(const Json &d)
{
	OBSSourceAutoRelease source = obs_get_source_by_name(text(d, "sourceName").toUtf8().constData());
	if (!source)
		return fail(QStringLiteral("No source named that"), 601);
	uint32_t sw = obs_source_get_base_width(source), sh = obs_source_get_base_height(source);
	if (sw == 0 || sh == 0)
		return fail(QStringLiteral("The source has no size yet"), 604);
	int width = d.value(QLatin1String("imageWidth")).toInt((int)sw), height = d.value(QLatin1String("imageHeight")).toInt(0);
	width = qBound(8, width, 4096);
	height = height > 0 ? qBound(8, height, 4096) : qMax(8, (int)((double)width * sh / sw));
	QImage image = renderSource(source, (uint32_t)width, (uint32_t)height);
	if (image.isNull())
		return fail(QStringLiteral("Could not render the source"));
	QString format = text(d, "imageFormat").toLower();
	if (format.isEmpty())
		format = QStringLiteral("png");
	if (format != QLatin1String("png") && format != QLatin1String("jpg") && format != QLatin1String("jpeg"))
		return fail(QStringLiteral("Unsupported image format"), 602);
	const bool png = format == QLatin1String("png");
	QByteArray bytes;
	QBuffer buffer(&bytes);
	buffer.open(QIODevice::WriteOnly);
	if (!(png ? image : image.convertToFormat(QImage::Format_RGB32)).save(&buffer, png ? "PNG" : "JPG", d.value(QLatin1String("imageCompressionQuality")).toInt(-1)))
		return fail(QStringLiteral("Could not encode the image"));
	return ok({{"imageData", QStringLiteral("data:image/%1;base64,%2").arg(png ? "png" : "jpeg", QString::fromLatin1(bytes.toBase64()))}});
}

const QHash<QString, std::function<Reply(const Json &)>> &handlers()
{
	static const QHash<QString, std::function<Reply(const Json &)>> table = {
		{"GetVersion", getVersion},
		{"GetProfileList", getProfileList},
		{"GetCurrentProgramScene", getCurrentProgramScene},
		{"GetSceneList", getSceneList},
		{"SetCurrentProgramScene", setCurrentProgramScene},
		{"GetStreamStatus", getStreamStatus},
		{"GetStats", getStats},
		{"GetVideoSettings", getVideoSettings},
		{"GetStreamServiceSettings", getStreamServiceSettings},
		{"SetStreamServiceSettings", setStreamServiceSettings},
		{"StartStream", startStream},
		{"StopStream", stopStream},
		{"GetInputList", getInputList},
		{"GetInputSettings", getInputSettings},
		{"SetInputSettings", setInputSettings},
		{"GetInputMute", getInputMute},
		{"GetInputAudioTracks", getInputAudioTracks},
		{"TriggerMediaInputAction", triggerMediaInputAction},
		{"CreateScene", createScene},
		{"CreateInput", createInput},
		{"GetSourceScreenshot", getSourceScreenshot},
		{"OpenStartDialog", openStartDialog},
	};
	return table;
}

// ---- minimal HTTP/1.1 server (one request per connection)

constexpr int kMaxHeader = 16 * 1024;
constexpr int kMaxBody = 256 * 1024;

bool sameToken(const QByteArray &a, const QByteArray &b)
{
	if (a.size() != b.size())
		return false;
	int diff = 0;
	for (int i = 0; i < a.size(); i++)
		diff |= a[i] ^ b[i];
	return diff == 0;
}

class Connection : public QObject {
public:
	Connection(QTcpSocket *socket, const QByteArray &token, quint16 port) : QObject(socket), socket_(socket), token_(token), port_(port)
	{
		QObject::connect(socket_, &QTcpSocket::readyRead, this, [this] { read(); });
		QObject::connect(socket_, &QTcpSocket::disconnected, socket_, &QObject::deleteLater);
		// A client that never finishes its request is dropped; once the request is complete the handler may take
		// as long as it needs (a start waits for the user's confirmation inside OBS).
		idle_ = new QTimer(this);
		idle_->setSingleShot(true);
		QObject::connect(idle_, &QTimer::timeout, socket_, [s = QPointer<QTcpSocket>(socket_)] {
			if (s)
				s->abort();
		});
		idle_->start(15000);
	}

private:
	void respond(int status, const QByteArray &reason, const Json &body)
	{
		const QByteArray payload = QJsonDocument(body).toJson(QJsonDocument::Compact);
		QByteArray head = "HTTP/1.1 " + QByteArray::number(status) + " " + reason + "\r\nContent-Type: application/json\r\nContent-Length: " +
				  QByteArray::number(payload.size()) + "\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n";
		socket_->write(head + payload);
		socket_->disconnectFromHost();
	}

	void read()
	{
		buffer_ += socket_->readAll();
		const int end = buffer_.indexOf("\r\n\r\n");
		if (end < 0) {
			if (buffer_.size() > kMaxHeader)
				respond(431, "Request Header Fields Too Large", {{"error", "header too large"}});
			return;
		}
		const QList<QByteArray> lines = buffer_.left(end).split('\n');
		QHash<QByteArray, QByteArray> headers;
		for (int i = 1; i < lines.size(); i++) {
			const int colon = lines[i].indexOf(':');
			if (colon > 0)
				headers.insert(lines[i].left(colon).trimmed().toLower(), lines[i].mid(colon + 1).trimmed());
		}
		const int length = headers.value("content-length", "0").toInt();
		if (length < 0 || length > kMaxBody)
			return respond(413, "Payload Too Large", {{"error", "body too large"}});
		if (buffer_.size() < end + 4 + length)
			return; // wait for the rest of the body
		idle_->stop();
		const QByteArray requestLine = lines.value(0).trimmed();
		const QByteArray body = buffer_.mid(end + 4, length);
		dispatch(requestLine, headers, body);
	}

	void dispatch(const QByteArray &requestLine, const QHash<QByteArray, QByteArray> &headers, const QByteArray &body)
	{
		const QByteArray host = headers.value("host");
		if (host != "127.0.0.1:" + QByteArray::number(port_) && host != "localhost:" + QByteArray::number(port_))
			return respond(403, "Forbidden", {{"error", "bad host"}});
		if (headers.contains("origin"))
			return respond(403, "Forbidden", {{"error", "browser requests are not allowed"}});
		const QByteArray auth = headers.value("authorization");
		if (!auth.startsWith("Bearer ") || !sameToken(auth.mid(7), token_))
			return respond(401, "Unauthorized", {{"error", "bad token"}});
		if (requestLine != "POST /call HTTP/1.1" && requestLine != "POST /call HTTP/1.0")
			return respond(404, "Not Found", {{"error", "unknown route"}});
		QJsonParseError parse;
		const QJsonObject request = QJsonDocument::fromJson(body, &parse).object();
		if (parse.error != QJsonParseError::NoError)
			return respond(400, "Bad Request", {{"error", "invalid JSON"}});
		const QString name = text(request, "request");
		const auto it = handlers().constFind(name);
		if (it == handlers().constEnd())
			return respond(404, "Not Found", {{"error", "unknown request"}, {"code", 204}});
		const Reply r = (*it)(request.value(QLatin1String("data")).toObject());
		if (r.ok)
			return respond(200, "OK", r.data);
		respond(409, "Conflict", {{"error", r.error}, {"code", r.code}});
	}

	QTcpSocket *socket_;
	QByteArray token_;
	quint16 port_;
	QByteArray buffer_;
	QTimer *idle_ = nullptr;
};

class Server : public QTcpServer {
public:
	explicit Server(QByteArray token) : token_(std::move(token)) {}

protected:
	void incomingConnection(qintptr handle) override
	{
		auto *socket = new QTcpSocket(this);
		if (!socket->setSocketDescriptor(handle)) {
			delete socket;
			return;
		}
		new Connection(socket, token_, serverPort());
	}

private:
	QByteArray token_;
};

QPointer<Server> g_server;

QString discoveryPath()
{
	// TIKTOK_OBS_API_FILE exists only so a test copy of OBS does not overwrite the real one's discovery file.
	if (qEnvironmentVariableIsSet("TIKTOK_OBS_API_FILE"))
		return qEnvironmentVariable("TIKTOK_OBS_API_FILE");
	return QDir(qEnvironmentVariable("LOCALAPPDATA")).filePath(QStringLiteral("TikTokLiveOBS/obs-api.json"));
}

} // namespace

bool obs_api_start()
{
	if (g_server)
		return true;
	QByteArray token;
	for (int i = 0; i < 4; i++) {
		const quint64 part = QRandomGenerator::system()->generate64();
		token += QByteArray::number(part, 16).rightJustified(16, '0');
	}
	auto *server = new Server(token);
	if (!server->listen(QHostAddress::LocalHost, 0)) {
		blog(LOG_WARNING, "[livett] control API could not listen: %s", server->errorString().toUtf8().constData());
		delete server;
		return false;
	}
	g_server = server;
	const QString path = discoveryPath();
	QDir().mkpath(QFileInfo(path).absolutePath());
	QFile file(path);
	if (!file.open(QIODevice::WriteOnly | QIODevice::Truncate)) {
		blog(LOG_WARNING, "[livett] control API: cannot write %s", path.toUtf8().constData());
		return false;
	}
	file.write(QJsonDocument(Json{{"port", (int)server->serverPort()},
				      {"token", QString::fromLatin1(token)},
				      {"pid", (int)GetCurrentProcessId()},
				      {"started", QDateTime::currentDateTimeUtc().toString(Qt::ISODate)}})
			   .toJson());
	blog(LOG_INFO, "[livett] control API listening on 127.0.0.1:%d", (int)server->serverPort());
	return true;
}

void obs_api_stop()
{
	if (g_server) {
		g_server->close();
		delete g_server;
	}
	QFile::remove(discoveryPath());
}
