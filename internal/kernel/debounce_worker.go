package kernel

import (
	"context"
	"log/slog"
	"time"
	"unicode/utf8"

	"daidai/internal/core"
)

func (w *sessionWorker) dispatchQueued(event queuedEvent) {
	w.queued.Add(1)
	select {
	case w.processCh <- event:
	case <-w.closeCh:
		w.queued.Add(-1)
	}
}

func (w *sessionWorker) isCurrent(event queuedEvent) bool {
	w.manager.mu.Lock()
	defer w.manager.mu.Unlock()
	if event.generation != w.generation.Load() {
		return false
	}
	current := w.senderGenerations[event.event.SenderID]
	return event.senderGeneration == current
}

func (w *sessionWorker) processLoop() {
	defer w.manager.wg.Done()
	for {
		// Shutdown must not start another queued turn after closeCh is closed.
		select {
		case <-w.closeCh:
			return
		default:
		}

		select {
		case <-w.closeCh:
			return
		case event := <-w.processCh:
			select {
			case <-w.closeCh:
				w.queued.Add(-1)
				return
			default:
			}
			if !w.isCurrent(event) {
				w.queued.Add(-1)
				continue
			}
			queuedEvent := event
			upstreamValid := queuedEvent.event.StillValid
			queuedEvent.event.StillValid = func() bool { return w.isCurrent(queuedEvent) && (upstreamValid == nil || upstreamValid()) }
			w.handle(queuedEvent.event)
		}
	}
}

func (w *sessionWorker) handle(event *core.SensoryEvent) {
	defer func() {
		w.queued.Add(-1)
		if recovered := recover(); recovered != nil {
			slog.Error("debounce: session event handler panicked", "session", w.key.String(), "panic", recovered)
		}
	}()
	if w.handler != nil {
		w.handler(event)
	}
}

// Stop 停止所有防抖 Actor 协程
func (m *DebounceManager) Stop() {
	m.stop()
}

// Shutdown 收敛所有会话 Actor，并等待其调度协程退出或 ctx 超时。
func (m *DebounceManager) Shutdown(ctx context.Context) error {
	m.stop()

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *DebounceManager) stop() {
	m.mu.Lock()
	m.stopped = true
	m.workers.Range(func(key, value any) bool {
		if w, ok := value.(*sessionWorker); ok {
			w.close()
		}
		m.workers.Delete(key)
		return true
	})
	m.mu.Unlock()
}

// loop 单个用户的独立事件驱动循环
func (w *sessionWorker) loop(onIdleCleanup func() bool) {
	defer w.manager.wg.Done()
	var (
		pendingEvents []queuedEvent
		pendingRunes  int
		timer         *time.Timer
		timerCh       <-chan time.Time
	)

	idleTimer := time.NewTimer(w.config.IdleTimeout)
	defer idleTimer.Stop()

	for {
		select {
		case queued := <-w.msgCh:
			if queued.afterControl != nil {
				select {
				case <-queued.afterControl:
				case <-w.closeCh:
					return
				}
			}
			if !w.isCurrent(queued) {
				continue
			}
			ev := queued.event
			if len(pendingEvents) > 0 && (pendingEvents[0].generation != queued.generation || pendingEvents[0].senderGeneration != queued.senderGeneration) {
				pendingEvents = nil
				pendingRunes = 0
				if timer != nil {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer = nil
					timerCh = nil
				}
			}
			// A merged turn has one outgoing quote ID and one quoted context.
			// Flush before a different reply target so text is never attributed to
			// the wrong quoted message.
			if len(pendingEvents) > 0 && (pendingEvents[0].event.SenderID != ev.SenderID || !sameReplyContext(pendingEvents[0].event, ev)) {
				if timer != nil {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer = nil
					timerCh = nil
				}
				merged := mergeQueuedEvents(pendingEvents)
				pendingEvents = nil
				pendingRunes = 0
				w.dispatchQueued(merged)
			}

			// Unaddressed group messages are history records (and occasionally
			// candidates for a short interjection), not ordinary turns to merge
			// with an @-addressed request. Dispatch each one separately so every
			// message is retained with its own platform message ID.
			if ev.GroupMessageObserved {
				if timer != nil {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer = nil
					timerCh = nil
				}
				if len(pendingEvents) > 0 {
					w.dispatchQueued(mergeQueuedEvents(pendingEvents))
					pendingEvents = nil
					pendingRunes = 0
				}
				if !idleTimer.Stop() {
					select {
					case <-idleTimer.C:
					default:
					}
				}
				idleTimer.Reset(w.config.IdleTimeout)
				w.dispatchQueued(queued)
				continue
			}

			eventRunes := utf8.RuneCountInString(ev.Content)
			if len(pendingEvents) > 0 && pendingRunes+eventRunes+1 > w.config.MaxMergedRunes {
				if timer != nil {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer = nil
					timerCh = nil
				}
				w.dispatchQueued(mergeQueuedEvents(pendingEvents))
				pendingEvents = nil
				pendingRunes = 0
			}

			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(w.config.IdleTimeout)

			pendingEvents = append(pendingEvents, queued)
			if pendingRunes > 0 {
				pendingRunes++
			}
			pendingRunes += eventRunes

			if timer == nil {
				timer = time.NewTimer(w.config.MergeWindow)
				timerCh = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(w.config.MergeWindow)
			}
			if len(pendingEvents) >= maxMergedEventsPerTurn {
				if timer != nil {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer = nil
					timerCh = nil
				}
				w.dispatchQueued(mergeQueuedEvents(pendingEvents))
				pendingEvents = nil
				pendingRunes = 0
			}

		case <-timerCh:
			timer = nil
			timerCh = nil
			if len(pendingEvents) == 0 {
				continue
			}
			if !w.isCurrent(pendingEvents[0]) {
				pendingEvents = nil
				pendingRunes = 0
				continue
			}

			merged := mergeQueuedEvents(pendingEvents)
			pendingEvents = nil
			pendingRunes = 0
			w.dispatchQueued(merged)

		case <-idleTimer.C:
			// Push completes its send before decrementing pushers, but a queued
			// message can remain in msgCh after that decrement. Do not retire the
			// actor while the input buffer still contains work.
			if len(pendingEvents) == 0 && len(w.msgCh) == 0 && w.queued.Load() == 0 && w.pushers.Load() == 0 {
				if onIdleCleanup() {
					return
				}
			}
			idleTimer.Reset(w.config.IdleTimeout)

		case <-w.closeCh:
			return
		}
	}
}
