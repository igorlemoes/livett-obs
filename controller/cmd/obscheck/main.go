// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// obscheck exercises the plugin's OBS control API (the controller's only way to reach OBS).
//
//	obscheck            read-only: version, profile, scenes, stream state, service type, stats, inputs
//	obscheck -write     also creates a scene and a media input, edits it, takes a screenshot and round-trips the
//	                    stream service settings. Changes the OBS it talks to: use it only on a test copy of OBS.
//
// It never starts or stops a stream and never prints a stream key.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"tiktoklive/internal/obs"
)

type step struct {
	failed int
}

func (s *step) check(name string, ok bool, detail any) {
	mark := "ok   "
	if !ok {
		mark = "FALHA"
		s.failed++
	}
	fmt.Printf("%s %-34s %v\n", mark, name, detail)
}

func main() {
	write := flag.Bool("write", false, "also run the operations that change OBS (test copies only)")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := obs.Open(ctx)
	if err != nil {
		fmt.Println("OBS:", err)
		os.Exit(1)
	}
	defer client.Close()
	call := func(name string, data map[string]any) map[string]any {
		out, err := client.Call(ctx, name, data)
		if err != nil {
			fmt.Printf("FALHA %-34s %v\n", name, err)
			return nil
		}
		return out
	}
	s := &step{}

	v := call("GetVersion", nil)
	s.check("GetVersion", v != nil && v["obsVersion"] != nil, v["obsVersion"])
	p := call("GetProfileList", nil)
	s.check("GetProfileList", p != nil && p["currentProfileName"] != "", p["currentProfileName"])
	cur := call("GetCurrentProgramScene", nil)
	scene, _ := cur["currentProgramSceneName"].(string)
	s.check("GetCurrentProgramScene", scene != "" || cur != nil, scene)
	list := call("GetSceneList", nil)
	scenes, _ := list["scenes"].([]any)
	s.check("GetSceneList", list != nil, fmt.Sprintf("%d cenas", len(scenes)))
	st := call("GetStreamStatus", nil)
	s.check("GetStreamStatus", st != nil && st["outputActive"] != nil, fmt.Sprintf("ativo=%v reconectando=%v", st["outputActive"], st["outputReconnecting"]))
	svc := call("GetStreamServiceSettings", nil)
	s.check("GetStreamServiceSettings", svc != nil && svc["streamServiceType"] != nil, svc["streamServiceType"])
	stats := call("GetStats", nil)
	s.check("GetStats", stats != nil && stats["activeFps"] != nil, fmt.Sprintf("fps=%.1f", num(stats["activeFps"])))
	vs := call("GetVideoSettings", nil)
	s.check("GetVideoSettings", vs != nil && num(vs["baseWidth"]) > 0, fmt.Sprintf("%vx%v", vs["baseWidth"], vs["baseHeight"]))
	inputs := call("GetInputList", nil)
	items, _ := inputs["inputs"].([]any)
	s.check("GetInputList", inputs != nil, fmt.Sprintf("%d fontes", len(items)))
	if len(items) > 0 {
		name, _ := items[0].(map[string]any)["inputName"].(string)
		s.check("GetInputSettings", call("GetInputSettings", map[string]any{"inputName": name}) != nil, "primeira fonte")
		s.check("GetInputMute", call("GetInputMute", map[string]any{"inputName": name}) != nil, "")
		s.check("GetInputAudioTracks", call("GetInputAudioTracks", map[string]any{"inputName": name}) != nil, "")
	}
	if scene != "" {
		shot := call("GetSourceScreenshot", map[string]any{"sourceName": scene, "imageFormat": "jpg", "imageWidth": 320, "imageCompressionQuality": 70})
		data, _ := shot["imageData"].(string)
		raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(data, "data:image/jpeg;base64,"))
		s.check("GetSourceScreenshot (jpg)", strings.HasPrefix(data, "data:image/jpeg;base64,") && len(raw) > 4 && raw[0] == 0xFF && raw[1] == 0xD8, fmt.Sprintf("%d bytes", len(raw)))
	}
	_, err = client.Call(ctx, "SetCurrentProgramScene", map[string]any{"sceneName": "cena que não existe"})
	s.check("cena inexistente é recusada", err != nil, err)
	_, err = client.Call(ctx, "NaoExiste", nil)
	s.check("pedido desconhecido é recusado", err != nil, err)

	if *write {
		fmt.Println("--- operações que alteram o OBS")
		name := fmt.Sprintf("obscheck-%d", time.Now().Unix())
		s.check("CreateScene", call("CreateScene", map[string]any{"sceneName": name}) != nil, name)
		_, dup := client.Call(ctx, "CreateScene", map[string]any{"sceneName": name})
		s.check("cena duplicada é recusada", dup != nil, "")
		in := name + " (vídeo)"
		made := call("CreateInput", map[string]any{"sceneName": name, "inputName": in, "inputKind": "ffmpeg_source",
			"inputSettings": map[string]any{"local_file": "C:\\nao-existe.mp4", "is_local_file": true, "looping": true}, "sceneItemEnabled": true})
		s.check("CreateInput (ffmpeg_source)", made != nil && made["sceneItemId"] != nil, made["sceneItemId"])
		kinds := call("GetInputList", map[string]any{"inputKind": "ffmpeg_source"})
		found := false
		for _, it := range asList(kinds["inputs"]) {
			if it.(map[string]any)["inputName"] == in {
				found = true
			}
		}
		s.check("GetInputList filtra por tipo", found, "")
		s.check("SetInputSettings", call("SetInputSettings", map[string]any{"inputName": in, "inputSettings": map[string]any{"looping": false}}) != nil, "")
		got := call("GetInputSettings", map[string]any{"inputName": in})
		settings, _ := got["inputSettings"].(map[string]any)
		s.check("SetInputSettings foi aplicado", settings["looping"] == false && settings["local_file"] == "C:\\nao-existe.mp4", settings["looping"])
		s.check("TriggerMediaInputAction", call("TriggerMediaInputAction", map[string]any{"inputName": in, "mediaAction": "OBS_WEBSOCKET_MEDIA_INPUT_ACTION_RESTART"}) != nil, "")
		s.check("SetCurrentProgramScene", call("SetCurrentProgramScene", map[string]any{"sceneName": name}) != nil, name)
		now := call("GetCurrentProgramScene", nil)
		s.check("a cena mudou de fato", now["currentProgramSceneName"] == name, now["currentProgramSceneName"])
		if scene != "" {
			call("SetCurrentProgramScene", map[string]any{"sceneName": scene})
		}

		before := call("GetStreamServiceSettings", nil)
		typ, _ := before["streamServiceType"].(string)
		oldSettings, _ := before["streamServiceSettings"].(map[string]any)
		s.check("SetStreamServiceSettings (mesmo tipo)", call("SetStreamServiceSettings", map[string]any{"streamServiceType": typ, "streamServiceSettings": oldSettings}) != nil, typ)
		custom := map[string]any{"server": "rtmp://127.0.0.1:1/obscheck", "key": "obscheck-key"}
		s.check("SetStreamServiceSettings (rtmp_custom)", call("SetStreamServiceSettings", map[string]any{"streamServiceType": "rtmp_custom", "streamServiceSettings": custom}) != nil, "")
		now2 := call("GetStreamServiceSettings", nil)
		ss, _ := now2["streamServiceSettings"].(map[string]any)
		s.check("o destino mudou de fato", now2["streamServiceType"] == "rtmp_custom" && ss["server"] == custom["server"] && ss["key"] == custom["key"], now2["streamServiceType"])
		s.check("tipo desconhecido é recusado", func() bool {
			_, err := client.Call(ctx, "SetStreamServiceSettings", map[string]any{"streamServiceType": "nao_existe", "streamServiceSettings": map[string]any{}})
			return err != nil
		}(), "")
		s.check("restaura o destino original", call("SetStreamServiceSettings", map[string]any{"streamServiceType": typ, "streamServiceSettings": oldSettings}) != nil, typ)
		_, stopErr := client.Call(ctx, "StopStream", nil)
		s.check("StopStream sem transmissão é recusado", stopErr != nil, "")
	}
	if s.failed > 0 {
		fmt.Printf("\n%d verificação(ões) falharam\n", s.failed)
		os.Exit(1)
	}
	fmt.Println("\ntudo certo")
}

func num(v any) float64 { f, _ := v.(float64); return f }

func asList(v any) []any { l, _ := v.([]any); return l }
