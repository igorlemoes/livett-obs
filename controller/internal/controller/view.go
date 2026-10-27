// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"strings"
	"time"

	"tiktoklive/internal/wire"
)

type comboKey struct{ user, gift, group string }

type combo struct {
	at   time.Time
	gift wire.Gift
}

// view is the live audience view (viewer counts, counters and the feed shown in the panel). Display only: nothing
// derived from viewer text ever triggers an action.
type view struct {
	viewers, total                                       any // nil until known
	stats                                                Obj
	feed                                                 []Obj // last 80
	seen                                                 []string
	seenSet                                              map[string]bool
	counts                                               map[string]int
	intro                                                *string
	seq                                                  int
	likes, comments, gifts, coins, follows, shares, fans uint64
	senders                                              map[string]bool
	combos                                               map[comboKey]combo
}

func newView() *view {
	return &view{stats: Obj{}, seenSet: map[string]bool{}, counts: map[string]int{}, senders: map[string]bool{}, combos: map[comboKey]combo{}}
}

func (v *view) push(item Obj) {
	v.seq++
	item["n"] = v.seq
	v.feed = append(v.feed, item)
	if len(v.feed) > 80 {
		v.feed = v.feed[len(v.feed)-80:]
	}
}

func (v *view) markSeen(id string) bool {
	if v.seenSet[id] {
		return false
	}
	v.seen = append(v.seen, id)
	v.seenSet[id] = true
	if len(v.seen) > 400 {
		delete(v.seenSet, v.seen[0])
		v.seen = v.seen[1:]
	}
	return true
}

func nameOf(u wire.User) string {
	if u.Name != "" {
		return u.Name
	}
	return u.Handle
}

// ingest turns decoded IM messages into the viewer/chat feed. Must be called with c.st held.
func (c *Controller) ingestLocked(response *wire.Response) {
	v := c.view
	for _, m := range response.Messages {
		if m.History || !v.markSeen(m.MessageID) {
			continue
		}
		method := strings.TrimPrefix(m.Method, "Webcast")
		v.counts[method]++
		switch method {
		case "ChatMessage":
			chat, err := wire.DecodeChat(m.Payload)
			if err != nil {
				continue
			}
			v.comments++
			v.push(Obj{"k": "chat", "name": nameOf(chat.User), "text": chat.Text, "avatar": chat.User.Avatar})
		case "LiveIntroMessage":
			if intro, err := wire.DecodeIntro(m.Payload); err == nil {
				v.intro = &intro
			}
		case "SocialMessage":
			if social, err := wire.DecodeSocial(m.Payload); err == nil {
				c.socialLocked(social)
			}
		case "RoomMessage":
			fan, err := wire.DecodeFanJoined(m.Payload)
			if err != nil || fan == nil {
				continue
			}
			v.fans++
			v.push(Obj{"k": "fan", "name": nameOf(fan.User), "rank": fan.Rank, "avatar": fan.User.Avatar})
		case "GiftMessage":
			if gift, err := wire.DecodeGift(m.Payload); err == nil {
				c.giftLocked(*gift)
			}
		case "LikeMessage":
			if like, err := wire.DecodeLike(m.Payload); err == nil && like.Total > v.likes {
				v.likes = like.Total
			}
		case "MemberMessage":
			member, err := wire.DecodeMember(m.Payload)
			if err != nil {
				continue
			}
			v.push(Obj{"k": "join", "name": nameOf(*member), "avatar": member.Avatar})
		case "RoomUserSeqMessage":
			counts, err := wire.DecodeRoomUsers(m.Payload)
			if err != nil {
				continue
			}
			v.viewers = counts.Viewers
			if counts.Total != 0 {
				v.total = counts.Total
			}
		}
	}
}

// socialLocked counts a follow/share by a real user; the host-guidance prompts have no user and are ignored.
func (c *Controller) socialLocked(s *wire.Social) {
	v := c.view
	if s.User == nil {
		return
	}
	var kind string
	switch {
	// Only event keys seen in real captures count. 'pm_mt_guidance_host_share' is a prompt TO the host
	// ("encourage your viewers to share") and mentions the host himself: it is not a share.
	case s.Action == 1 && strings.HasPrefix(s.Key, "pm_main_follow"):
		kind = "follow"
		v.follows++
	case s.Action == 3 && s.Key == "pm_mt_guidance_host_more_share":
		kind = "share"
		v.shares++
	default:
		return
	}
	v.push(Obj{"k": kind, "name": nameOf(*s.User), "avatar": s.User.Avatar})
}

// giftLocked: a combo streams several GiftMessages; count it once, when it ends (or after 10 s without an end).
func (c *Controller) giftLocked(g wire.Gift) {
	v := c.view
	key := comboKey{g.User.ID, g.GiftID, g.Group}
	v.combos[key] = combo{time.Now(), g}
	if g.Ended {
		c.flushGiftLocked(key)
	}
	for old, entry := range v.combos {
		if time.Since(entry.at) > 10*time.Second {
			c.flushGiftLocked(old)
		}
	}
}

func (c *Controller) flushGiftLocked(key comboKey) {
	v := c.view
	entry, ok := v.combos[key]
	if !ok {
		return
	}
	delete(v.combos, key)
	g := entry.gift
	v.gifts += g.Count
	v.coins += g.Count * g.Coins
	v.senders[g.User.ID] = true
	v.push(Obj{"k": "gift", "name": nameOf(g.User), "gift": g.Name, "count": g.Count, "coins": g.Count * g.Coins, "avatar": g.User.Avatar})
}

func maxFloat(stat any, local uint64) any {
	cur := numOr(stat, 0)
	if cur > float64(local) {
		return normalize(stat)
	}
	return local
}

// viewSnapshotLocked is what the panel shows. Must be called with c.st held.
func (c *Controller) viewSnapshotLocked() Obj {
	v := c.view
	if v.seq == 0 && len(v.stats) == 0 && v.viewers == nil {
		return nil
	}
	// room/info counters for likes/comments are not updated live; the IM stream is the source of truth.
	stats := Obj{}
	for k, x := range v.stats {
		stats[k] = normalize(x)
	}
	stats["likes"] = maxFloat(v.stats["likes"], v.likes)
	stats["comments"] = maxFloat(v.stats["comments"], v.comments)
	// room/info keeps follow_count live but not shares; the IM events fill the gaps.
	if v.stats["follows"] == nil {
		stats["follows"] = v.follows
	}
	stats["shares"] = maxFloat(v.stats["shares"], v.shares)
	stats["fans"] = v.fans
	stats["gifts"] = v.gifts
	stats["coins"] = v.coins
	stats["gifters"] = maxFloat(v.stats["gifters"], uint64(len(v.senders)))
	start := 0
	if len(v.feed) > 50 {
		start = len(v.feed) - 50
	}
	feed := make([]any, 0, len(v.feed)-start)
	for _, item := range v.feed[start:] {
		feed = append(feed, item)
	}
	counts := Obj{}
	for k, n := range v.counts {
		counts[k] = n
	}
	var intro any
	if v.intro != nil {
		intro = *v.intro
	}
	return Obj{"viewers": v.viewers, "total": v.total, "stats": stats, "intro": intro, "feed": feed, "counts": counts}
}
