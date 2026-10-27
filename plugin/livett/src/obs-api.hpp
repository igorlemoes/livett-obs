// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Local control API of the plugin: what the controller used to ask obs-websocket for, answered from inside OBS.
//
// Loopback only, one random token per OBS run, published in %LOCALAPPDATA%\TikTokLiveOBS\obs-api.json (private to the
// Windows user). Requests are POST /call {"request": "<Name>", "data": {...}} and use the obs-websocket request names
// and response fields the controller already understands, so the controller changes very little.
#pragma once

// Starts listening (on the OBS UI thread, where the frontend API must be used). Returns false when it cannot.
bool obs_api_start();

// Stops listening and removes the discovery file.
void obs_api_stop();
