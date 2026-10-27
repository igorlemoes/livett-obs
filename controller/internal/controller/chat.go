// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type chatEntry struct {
	at      time.Time
	content string
}

var (
	hiddenChars = regexp.MustCompile(`[\x00-\x1f\x7f\x{200b}-\x{200f}\x{202a}-\x{202e}\x{2060}\x{feff}]+`)
	linkLike    = regexp.MustCompile(`(?i)(https?:|www\.|\b[a-z0-9-]+\.(com|net|org|br|io|me|ly|gg|tv|app|xyz|link)\b|t\.me/|wa\.me/)`)
	nameJunk    = regexp.MustCompile(`[@\s\x00-\x1f]+`)
)

// chatContentLocked validates and normalises an outgoing chat message. Viewer text is never executed, only quoted
// by name taken from the feed (never from the caller). c.st must be held (it reads the feed).
func (c *Controller) chatContentLocked(text any, replyToN any) (string, error) {
	s, ok := text.(string)
	if !ok {
		return "", perr("Informe o texto da mensagem.")
	}
	content := hiddenChars.ReplaceAllString(s, " ")
	content = strings.Join(strings.Fields(content), " ")
	if content == "" {
		return "", perr("A mensagem está vazia.")
	}
	if linkLike.MatchString(content) {
		return "", perr("Mensagens com links ou endereços não são enviadas.")
	}
	if replyToN != nil {
		n, ok := replyToN.(int)
		if !ok {
			return "", perr("Número de mensagem inválido.")
		}
		var item Obj
		for _, m := range c.view.feed {
			if m["n"] == n {
				item = m
				break
			}
		}
		name, _ := item["name"].(string)
		name = strings.TrimSpace(nameJunk.ReplaceAllString(name, " "))
		if r := []rune(name); len(r) > 30 {
			name = strings.TrimSpace(string(r[:30]))
		}
		if item == nil || name == "" {
			return "", perr("Não encontrei essa mensagem no chat para responder.")
		}
		content = "@" + name + " " + content
	}
	if utf8.RuneCountInString(content) > c.ChatMax {
		return "", perrf("A mensagem passa de %d caracteres.", c.ChatMax)
	}
	return content, nil
}

// ChatRequest: Via is "", "thanks" or "answer" (the last two are written by the assistant under the user's
// standing permission for this LIVE).
type ChatRequest struct {
	Text    any
	ReplyTo any // nil or int: the feed number being answered
	Via     string
	Context any
}

// SendChat sends one chat message as the host (live only, rate limited, no links, no repeats).
func (c *Controller) SendChat(ctx context.Context, req ChatRequest) (Obj, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.st.Lock()
	if c.phase != Live || c.binding == nil {
		c.st.Unlock()
		return nil, perr("Só é possível enviar mensagens ao chat durante a LIVE.")
	}
	auto := req.Via == "thanks" || req.Via == "answer"
	switch {
	case req.Via != "" && !auto:
		c.st.Unlock()
		return nil, perr("Origem da mensagem inválida.")
	case req.Via == "thanks" && !c.autopilot["thanks"]:
		c.st.Unlock()
		return nil, perr("Os agradecimentos automáticos não foram autorizados nesta LIVE.")
	case req.Via == "answer" && !c.autopilot["answers"]:
		c.st.Unlock()
		return nil, perr("As respostas automáticas não foram autorizadas nesta LIVE.")
	}
	content, err := c.chatContentLocked(req.Text, req.ReplyTo)
	c.st.Unlock()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	minInterval, perMinute := c.ChatMinInterval, c.ChatPerMinute
	if auto {
		minInterval, perMinute = c.AutoMinInterval, c.AutoPerMinute
		if c.autoSent >= c.AutoMaxPerLive {
			return nil, perr("Limite de respostas automáticas desta LIVE atingido.")
		}
	}
	if n := len(c.chatLog); n > 0 && now.Sub(c.chatLog[n-1].at) < minInterval {
		return nil, perr("Aguarde alguns segundos entre uma mensagem e outra.")
	}
	recent := 0
	for _, e := range c.chatLog {
		if now.Sub(e.at) < time.Minute {
			recent++
		}
	}
	if recent >= perMinute {
		return nil, perr("Limite de mensagens por minuto atingido; aguarde.")
	}
	for _, e := range c.chatLog {
		if e.content == content && now.Sub(e.at) < c.ChatRepeatWindow {
			return nil, perr("Essa mensagem já foi enviada há pouco; não repito texto.")
		}
	}
	result, err := c.browser.Call(ctx, "chat", Obj{"room_id": c.binding.RoomID, "content": content})
	if err != nil {
		return nil, err
	}
	r := asObj(result)
	if !isZero(r["code"]) {
		prompt, _ := r["prompts"].(string)
		if auto {
			c.autoFailures++
			c.audit("failed", Obj{"via": req.Via, "error": nilIfEmpty(prompt)})
			if c.autoFailures >= 3 { // kill switch: something is wrong, stop talking on its own
				c.st.Lock()
				c.autopilot = map[string]bool{"thanks": false, "answers": false}
				c.detail = "Respostas automáticas desligadas após 3 recusas seguidas do TikTok."
				c.st.Unlock()
				c.audit("autopilot_off_after_failures", Obj{})
			}
		}
		if prompt == "" {
			prompt = "O TikTok não aceitou a mensagem."
		}
		return nil, perr(prompt)
	}
	c.chatLog = append(c.chatLog, chatEntry{now, content})
	if len(c.chatLog) > 30 {
		c.chatLog = c.chatLog[len(c.chatLog)-30:]
	}
	var via any
	if req.Via != "" {
		via = req.Via
	}
	c.emit("controller", "chat_sent", Obj{"length": utf8.RuneCountInString(content), "reply": req.ReplyTo != nil, "via": via})
	if auto {
		c.autoSent++
		c.autoFailures = 0
		c.audit(req.Via, Obj{"sent": content, "reply_to_n": req.ReplyTo, "context": req.Context})
	}
	return Obj{"sent": content}, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// audit logs every automatic message with the event/comment it answered (best effort).
func (c *Controller) audit(kind string, data Obj) {
	line := Obj{"at": epoch(time.Now()), "kind": kind}
	for k, v := range data {
		line[k] = v
	}
	raw, err := json.Marshal(line)
	if err != nil {
		return
	}
	path := filepath.Join(c.root, "runtime", "autopilot-log.jsonl")
	os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(raw, '\n'))
}

// SetAutopilot records the user's explicit permission for the CURRENT broadcast only (turning it off is always
// allowed). A nil argument leaves that switch unchanged.
func (c *Controller) SetAutopilot(thanks, answers *bool) (Obj, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if (thanks != nil && *thanks) || (answers != nil && *answers) {
		if c.getPhase() != Live {
			return nil, perr("Só é possível autorizar durante a LIVE (a permissão vale apenas para ela).")
		}
	}
	c.st.Lock()
	if thanks != nil {
		c.autopilot["thanks"] = *thanks
	}
	if answers != nil {
		c.autopilot["answers"] = *answers
	}
	state := Obj{"thanks": c.autopilot["thanks"], "answers": c.autopilot["answers"]}
	c.autoFailures = 0
	c.st.Unlock()
	c.audit("autopilot_set", state)
	c.emit("controller", "autopilot", state)
	return c.Snapshot(), nil
}

// resetAutopilotLocked: the permission ends with the broadcast. c.st must be held.
func (c *Controller) resetAutopilotLocked() {
	c.autopilot = map[string]bool{"thanks": false, "answers": false}
	c.autoSent = 0
	c.autoFailures = 0
}
