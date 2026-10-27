// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package telemetry

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type event struct {
	T    float64
	Src  string
	Kind string
	Data Obj
}

func readEvents(dir string) ([]event, error) {
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []event
	reader := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var raw struct {
				T    float64 `json:"t"`
				Src  string  `json:"src"`
				Kind string  `json:"kind"`
				Data Obj     `json:"data"`
			}
			if json.Unmarshal(line, &raw) == nil {
				out = append(out, event{raw.T, raw.Src, raw.Kind, raw.Data})
			}
		}
		if err != nil {
			return out, nil
		}
	}
}

func num(v any) (float64, bool) { f, ok := v.(float64); return f, ok }

func rangeOf(values []any) string {
	var nums []float64
	for _, v := range values {
		if f, ok := num(v); ok {
			nums = append(nums, f)
		}
	}
	if len(nums) == 0 {
		return "sem dados"
	}
	lo, hi, sum := nums[0], nums[0], 0.0
	for _, f := range nums {
		if f < lo {
			lo = f
		}
		if f > hi {
			hi = f
		}
		sum += f
	}
	return fmt.Sprintf("%.6g / %.1f / %.6g", lo, sum/float64(len(nums)), hi)
}

// counter keeps first-seen order, (it remembers the order in which values were first seen).
type counter struct {
	keys []string
	n    map[string]int
}

func newCounter() *counter { return &counter{n: map[string]int{}} }

func (c *counter) add(key string) {
	if _, ok := c.n[key]; !ok {
		c.keys = append(c.keys, key)
	}
	c.n[key]++
}

func (c *counter) sortedKeys() []string {
	keys := append([]string{}, c.keys...)
	sort.Strings(keys)
	return keys
}

// mostCommon: count descending, ties in first-seen order (Counter.most_common); limit <= 0 means all.
func (c *counter) mostCommon(limit int) []string {
	keys := append([]string{}, c.keys...)
	sort.SliceStable(keys, func(i, j int) bool { return c.n[keys[i]] > c.n[keys[j]] })
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	return keys
}

// BuildSummary renders a readable report from a trace directory.
func BuildSummary(dir string) string {
	events, err := readEvents(dir)
	out := []string{"# Resumo do trace " + filepath.Base(dir), ""}
	if err != nil || len(events) == 0 {
		return strings.Join(append(out, "Sem eventos."), "\n")
	}
	out = append(out, fmt.Sprintf("Duração: %.0f s · eventos: %d", events[len(events)-1].T, len(events)))
	counts := newCounter()
	for _, e := range events {
		counts.add(e.Src + "." + e.Kind)
	}
	out = append(out, "", "## Contagem por fonte")
	for _, k := range counts.sortedKeys() {
		out = append(out, fmt.Sprintf("- %s: %d", k, counts.n[k]))
	}

	out = append(out, "", "## Linha do tempo do controlador")
	last := ""
	for _, e := range events {
		if e.Src == "controller" && e.Kind == "phase" {
			if phase, _ := e.Data["phase"].(string); phase != last {
				last = phase
				out = append(out, fmt.Sprintf("- %.1fs → %s: %v", e.T, last, orEmpty(e.Data["detail"])))
			}
		}
	}
	for _, e := range events {
		if e.Src == "browser" && e.Kind == "call" {
			var code any
			if r, ok := e.Data["result"].(map[string]any); ok {
				if code = r["code"]; code == nil {
					code = r["status_code"]
				}
			}
			out = append(out, fmt.Sprintf("- %.1fs browser.%v %v ms código=%v erro=%v", e.T, e.Data["name"], e.Data["ms"], pyNone(code), pyNone(e.Data["error"])))
		}
	}

	http := newCounter()
	failures := 0
	for _, e := range events {
		if e.Src == "cdp" && e.Kind == "response" {
			path := ""
			if u, err := url.Parse(fmt.Sprint(e.Data["url"])); err == nil {
				path = u.Path
			}
			http.add(fmt.Sprintf("%s → %v", path, e.Data["status"]))
		}
		if e.Src == "cdp" && (e.Kind == "failed" || e.Kind == "exception") {
			failures++
		}
	}
	out = append(out, "", "## Requisições HTTP observadas no Chrome")
	for _, k := range http.sortedKeys() {
		out = append(out, fmt.Sprintf("- %s: %dx", k, http.n[k]))
	}
	out = append(out, fmt.Sprintf("Falhas/exceções no navegador: %d", failures))

	var stats []Obj
	for _, e := range events {
		if e.Src == "obs" && e.Kind == "stats" {
			stats = append(stats, e.Data)
		}
	}
	if len(stats) > 0 {
		pick := func(group, key string, from int) []any {
			var v []any
			for _, d := range stats[from:] {
				v = append(v, asObj(d[group])[key])
			}
			return v
		}
		lastStats, lastStream := asObj(stats[len(stats)-1]["stats"]), asObj(stats[len(stats)-1]["stream"])
		var kbps []any
		for _, d := range stats[1:] {
			kbps = append(kbps, d["kbps"])
		}
		out = append(out, "", "## OBS (mín / média / máx)",
			"- CPU %: "+rangeOf(pick("stats", "cpuUsage", 0)),
			"- FPS ativo: "+rangeOf(pick("stats", "activeFps", 0)),
			"- Bitrate kbps: "+rangeOf(kbps),
			"- Congestionamento: "+rangeOf(pick("stream", "outputCongestion", 0)),
			fmt.Sprintf("- Quadros perdidos na saída (final): %v de %v", pyNone(lastStream["outputSkippedFrames"]), pyNone(lastStream["outputTotalFrames"])),
			fmt.Sprintf("- Quadros de render perdidos (final): %v de %v", pyNone(lastStats["renderSkippedFrames"]), pyNone(lastStats["renderTotalFrames"])))
	}
	peaks := map[string][]any{}
	var peakNames []string
	obsEvents := newCounter()
	conns := newCounter()
	for _, e := range events {
		switch {
		case e.Src == "obs" && e.Kind == "audio_peaks":
			for name, db := range asObj(e.Data["peak_db"]) {
				if _, seen := peaks[name]; !seen {
					peakNames = append(peakNames, name)
				}
				peaks[name] = append(peaks[name], db)
			}
		case e.Src == "obs" && e.Kind == "event":
			obsEvents.add(fmt.Sprint(pyNone(e.Data["type"])))
		case e.Src == "obs_process" && e.Kind == "sample":
			if list, ok := e.Data["conns"].([]any); ok {
				for _, c := range list {
					m := asObj(c)
					conns.add(fmt.Sprintf("%v:%v %v", pyNone(m["RemoteAddress"]), pyNone(m["RemotePort"]), pyNone(m["State"])))
				}
			}
		}
	}
	sort.Strings(peakNames)
	for _, name := range peakNames {
		out = append(out, fmt.Sprintf(`- Áudio "%s" pico dB (mín/méd/máx): %s`, name, rangeOf(peaks[name])))
	}
	for _, k := range obsEvents.keys {
		out = append(out, fmt.Sprintf("- Evento OBS %s: %d", k, obsEvents.n[k]))
	}
	if len(conns.keys) > 0 {
		out = append(out, "", "## Conexões TCP do OBS (amostras)")
		for _, k := range conns.mostCommon(10) {
			out = append(out, fmt.Sprintf("- %s: %d", k, conns.n[k]))
		}
	}

	full, probeErrors := 0, 0
	changed := newCounter()
	for _, e := range events {
		if e.Src != "room" {
			continue
		}
		switch e.Kind {
		case "full":
			full++
		case "probe_error":
			probeErrors++
		}
		if list, ok := e.Data["changes"].([]any); ok {
			for _, c := range list {
				changed.add(fmt.Sprint(asObj(c)["path"]))
			}
		}
	}
	out = append(out, "", "## Estado da sala", fmt.Sprintf("Leituras completas: %d · erros: %d", full, probeErrors))
	for _, k := range changed.mostCommon(25) {
		out = append(out, fmt.Sprintf("- campo alterado %s: %dx", k, changed.n[k]))
	}

	batches, messages, imErrors := 0, 0, 0
	methods, fetchTypes, errKinds := newCounter(), newCounter(), newCounter()
	pushServers := map[string]bool{}
	for _, e := range events {
		if e.Src != "im" {
			continue
		}
		switch e.Kind {
		case "batch":
			batches++
			fetchTypes.add(fmt.Sprint(pyNone(e.Data["fetch_type"])))
			if ps, _ := e.Data["push_server"].(string); ps != "" {
				pushServers[ps] = true
			}
		case "message":
			messages++
			methods.add(fmt.Sprint(e.Data["method"]))
		case "error":
			imErrors++
			errKinds.add(fmt.Sprint(e.Data["error"]))
		}
	}
	out = append(out, "", "## Mensagens IM (somente observação)", fmt.Sprintf("Lotes: %d · mensagens: %d · erros: %d", batches, messages, imErrors))
	var ft []string
	for _, k := range fetchTypes.keys {
		ft = append(ft, fmt.Sprintf("%s: %d", k, fetchTypes.n[k]))
	}
	var servers []string
	for ps := range pushServers {
		servers = append(servers, ps)
	}
	sort.Strings(servers)
	quoted := make([]string, len(servers))
	for i, v := range servers {
		quoted[i] = "'" + v + "'"
	}
	out = append(out, "fetch_type: {"+strings.Join(ft, ", ")+"}", "Servidor de push oferecido: ["+strings.Join(quoted, ", ")+"]")
	for _, k := range methods.mostCommon(0) {
		out = append(out, fmt.Sprintf("- %s: %d", k, methods.n[k]))
	}
	for _, e := range events {
		if e.Src == "im" && e.Kind == "message" && e.Data["classified"] != nil {
			out = append(out, fmt.Sprintf("- sinal classificado %v → %v em %v", e.Data["method"], e.Data["classified"], e.Data["id"]))
		}
	}
	if imErrors > 0 {
		var top []string
		for _, k := range errKinds.mostCommon(3) {
			top = append(top, fmt.Sprintf("('%s', %d)", k, errKinds.n[k]))
		}
		out = append(out, "- erro IM: ["+strings.Join(top, ", ")+"]")
	}
	ws := 0
	for _, e := range events {
		if e.Src == "cdp" && strings.HasPrefix(e.Kind, "ws_") {
			ws++
		}
	}
	out = append(out, "", fmt.Sprintf("## WebSockets no navegador: %d eventos", ws))
	var collectors []string
	for _, e := range events {
		if e.Kind == "collector_error" {
			collectors = append(collectors, fmt.Sprint(e.Data))
		}
	}
	if len(collectors) > 0 {
		out = append(out, "Falhas de coletores: "+strings.Join(collectors, "; "))
	}
	return strings.Join(out, "\n") + "\n"
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func pyNone(v any) any {
	if v == nil {
		return "None"
	}
	return v
}

// WriteSummary writes summary.md next to the trace.
func WriteSummary(dir string) string {
	text := BuildSummary(dir)
	os.WriteFile(filepath.Join(dir, "summary.md"), []byte(text), 0o644)
	return text
}
