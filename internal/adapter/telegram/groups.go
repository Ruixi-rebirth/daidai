package telegram

import (
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

type groupState struct {
	lastSeen       time.Time
	lastConsidered time.Time
	timer          *time.Timer
	generation     uint64
	participants   []core.GroupParticipant
}

func (c *Channel) observeGroup(event *entity.SensoryEvent, replyingToBot bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopping || c.queue == nil {
		return
	}
	if c.groups == nil {
		c.groups = make(map[string]*groupState)
	}
	g := c.groups[event.ChannelID]
	if g == nil {
		if len(c.groups) >= 1024 {
			var oldest string
			for id, state := range c.groups {
				if oldest == "" || state.lastSeen.Before(c.groups[oldest].lastSeen) {
					oldest = id
				}
			}
			if c.groups[oldest].timer != nil {
				c.groups[oldest].timer.Stop()
			}
			delete(c.groups, oldest)
		}
		g = &groupState{}
		c.groups[event.ChannelID] = g
	}
	g.lastSeen = time.Now()
	g.generation++
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	participants := []core.GroupParticipant{{ID: event.SenderID, Name: event.SenderName}}
	for _, p := range g.participants {
		if p.ID != event.SenderID && len(participants) < 20 {
			participants = append(participants, p)
		}
	}
	g.participants = participants
	if !event.GroupMessageObserved {
		g.lastConsidered = time.Now()
		return
	}
	if replyingToBot || strings.TrimSpace(event.Content) == "" || strings.HasPrefix(event.Content, "/") || time.Since(g.lastConsidered) < 5*time.Minute {
		return
	}
	candidate := *event
	candidate.GroupInterjection = true
	candidate.GroupParticipants = append([]core.GroupParticipant(nil), participants...)
	generation := g.generation
	g.timer = time.AfterFunc(4*time.Second, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.stopping || c.groups[event.ChannelID] != g || g.generation != generation || time.Since(g.lastConsidered) < 5*time.Minute {
			return
		}
		g.lastConsidered = time.Now()
		g.timer = nil
		candidate.StillValid = func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			return !c.stopping && c.groups[event.ChannelID] == g && g.generation == generation
		}
		c.queue.Push(&candidate)
	})
}
