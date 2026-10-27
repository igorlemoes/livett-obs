// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Copia as páginas do celular que o controlador também embute (fonte única: controller/internal/assets/camera) para public/.
import { copyFileSync, mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const from = join(here, '..', '..', 'controller', 'internal', 'assets', 'camera');
const to = join(here, '..', 'public');
mkdirSync(join(to, 's'), { recursive: true });
copyFileSync(join(from, 'phone.html'), join(to, 'phone.html'));
copyFileSync(join(from, 'phone.js'), join(to, 's', 'phone.js'));
copyFileSync(join(from, 'camera.css'), join(to, 's', 'camera.css'));
console.log('páginas copiadas para public/');
