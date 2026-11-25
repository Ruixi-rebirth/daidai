package entity

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"
)

var personaZone = time.FixedZone("CST", 8*3600)

type circadianProfile struct {
	phase  string
	energy string
	tempo  string
}

var innerMoodPool = [...]string{
	"平和",
	"有点犯懒",
	"安静放空",
	"温柔但低功耗",
	"稍微有精神",
	"嘴硬但松弛",
	"偷偷开心",
}

func resolveCircadianProfile(hour int) circadianProfile {
	switch {
	case hour >= 0 && hour < 7:
		return circadianProfile{
			phase:  "深夜",
			energy: "安静夜猫子",
			tempo:  "语气轻软安静、话短一点但脑子清醒机灵、句尾自然收住",
		}
	case hour >= 7 && hour < 12:
		return circadianProfile{
			phase:  "上午",
			energy: "偏低",
			tempo:  "刚清醒不太热络、轻快一点但不亢奋",
		}
	case hour >= 12 && hour < 18:
		return circadianProfile{
			phase:  "午后",
			energy: "中等",
			tempo:  "自然松弛、软一点接话",
		}
	case hour >= 18 && hour < 23:
		return circadianProfile{
			phase:  "夜晚",
			energy: "偏高",
			tempo:  "稍微活泛有精神、保持软糯自然",
		}
	default:
		return circadianProfile{
			phase:  "夜深",
			energy: "下降中",
			tempo:  "话少一点、压低声音、不硬聊",
		}
	}
}

func stableWindowMood(local time.Time) string {
	key := fmt.Sprintf("%s:%d:inner_mood", local.Format("2006-01-02"), local.Hour()/3)
	sum := sha256.Sum256([]byte(key))
	idx := int(binary.BigEndian.Uint32(sum[:4]) % uint32(len(innerMoodPool)))
	return innerMoodPool[idx]
}

// CircadianRhythm returns a deterministic 3-hour-windowed background energy and
// mood note so Daidai's conversational tempo shifts naturally across the day
// without turning background props into repetitive catchphrases.
func CircadianRhythm(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	local := now.In(personaZone)
	profile := resolveCircadianProfile(local.Hour())
	mood := stableWindowMood(local)
	return fmt.Sprintf(
		"【此刻内在节律（仅在背后调节说话劲头与句长，不是聊天素材；除非对方直接问“在干嘛”，绝不主动说出状态词）】：%s · 精力%s · 心境%s · 节奏：%s",
		profile.phase, profile.energy, mood, profile.tempo,
	)
}
