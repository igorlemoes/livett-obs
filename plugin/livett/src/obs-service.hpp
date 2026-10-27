// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// The plugin's own OBS service, "LiveTT" (type id "tiktok_live"), and the "start a LIVE" window.
//
// While the plugin is loaded, OBS's "Iniciar transmissão" button starts a LIVE on TikTok: a click on it opens the start window
// (account, title, duration, context for the AI); on "Iniciar LIVE" the plugin takes the OBS button over FOR THAT START ONLY by
// putting its own service in OBS's memory (never saved: the profile's service.json is not touched), asks the controller to
// create the room, and gives OBS the destination through memory (get_connect_info), so the stream key is never written to the
// OBS profile. When the stream stops, the profile's own service is put back. The controller finishes the LIVE (guards,
// pings, watchdog) like the panel's own start. See docs/ARQUITETURA.md.
#pragma once

#include <obs-frontend-api.h>

// Registers the service type with OBS. Call from obs_module_load.
void tiktok_service_register();

// Makes a click on OBS's start button open the start window. Call once OBS has finished loading.
void tiktok_service_watch_start_button();

// Opens the same start window without a click (the panel's "Iniciar LIVE" button reaches it through the control API).
void tiktok_service_request_start();

// Frontend events the service must follow (stream stopped: forget the destination and give the profile its service back).
void tiktok_service_on_frontend_event(enum obs_frontend_event event);

// True while a start the plugin began is under way (window open, service swapped in): the controller must not touch the service then.
bool tiktok_service_start_in_progress();

// Shows the plugin's panel dock (implemented in plugin-main.cpp).
void tiktok_show_panel();
