package telegram

import (
	"strings"

	"daidai/internal/entity"
	"daidai/internal/groupchat"
)

func (c *Channel) interjectionScheduler() *groupchat.InterjectionScheduler {
	c.interjectionsOnce.Do(func() {
		if c.interjections == nil {
			c.interjections = groupchat.NewInterjectionScheduler(groupchat.InterjectionConfig{})
		}
	})
	return c.interjections
}

func (c *Channel) observeGroup(event *entity.SensoryEvent, replyingToBot bool) {
	c.mu.Lock()
	stopping := c.stopping
	queue := c.queue
	c.mu.Unlock()
	if stopping || queue == nil || event == nil {
		return
	}
	if !event.GroupMessageObserved {
		c.interjectionScheduler().NotifyAddressed(event.ChannelID, event.SenderID, event.SenderName)
		return
	}
	eligible := !replyingToBot && strings.TrimSpace(event.Content) != "" && !strings.HasPrefix(event.Content, "/")
	candidate := *event
	c.interjectionScheduler().Observe(event.ChannelID, &candidate, eligible, func(ev *entity.SensoryEvent) {
		c.mu.Lock()
		isStopping := c.stopping
		c.mu.Unlock()
		if isStopping {
			return
		}
		queue.Push(ev)
	})
}
