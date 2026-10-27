// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// The "Câmera do celular" source: it appears in OBS's Sources > + menu, and adding it opens a window with the QR code that pairs
// the phone. Inside it is a private Browser Source that loads the controller's receiver page (the phone's video and sound arrive
// there by WebRTC); its sound is passed on to this source so it shows up in the mixer. See docs/ARQUITETURA.md §7.2.
#pragma once

// Registers the source type with OBS. Call from obs_module_load.
void tiktok_phone_camera_register();
