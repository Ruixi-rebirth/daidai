package assets

import (
	"embed"
	"path"
	"strings"
)

// SmallFaces contains the bundled face stickers used as startup defaults.
//
//go:embed daidai_faces_small/*.png
var SmallFaces embed.FS

// BundledFaces lists all 34 built-in Daidai reaction stickers in display order.
var BundledFaces = [...]string{
	"开心", "无语", "害羞", "裂开", "得意", "星星眼", "困", "生气", "委屈",
	"吐舌", "震惊", "比心", "大笑", "大哭", "脸红", "流汗", "思考", "疑惑",
	"期待", "陶醉", "卖萌", "加油", "鼓掌", "惊吓", "生闷气", "捂脸", "白眼",
	"坏笑", "无奈", "晕", "嫌弃", "爱心", "哈欠", "点赞",
}

var bundledFaceSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(BundledFaces))
	for _, name := range BundledFaces {
		m[name] = struct{}{}
	}
	return m
}()

var bundledFaceAliases = map[string]string{
	"笑":   "开心",
	"哈哈":  "大笑",
	"哭":   "大哭",
	"汗":   "流汗",
	"擦汗":  "流汗",
	"困了":  "困",
	"打哈欠": "哈欠",
	"赞":   "点赞",
	"翻白眼": "白眼",
	"闷气":  "生闷气",
	"吓":   "惊吓",
}

// ResolveBundledFace maps an emotion token or short alias to a canonical bundled sticker name.
func ResolveBundledFace(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", false
	}
	if _, ok := bundledFaceSet[name]; ok {
		return name, true
	}
	if canonical, ok := bundledFaceAliases[name]; ok {
		return canonical, true
	}
	return "", false
}

// ReadBundledSmallFace returns the PNG bytes and canonical emotion name for a bundled sticker.
func ReadBundledSmallFace(raw string) ([]byte, string, bool) {
	emotion, ok := ResolveBundledFace(raw)
	if !ok {
		return nil, "", false
	}
	data, err := SmallFaces.ReadFile(path.Join("daidai_faces_small", emotion+".png"))
	if err != nil || len(data) == 0 {
		return nil, "", false
	}
	return data, emotion, true
}
