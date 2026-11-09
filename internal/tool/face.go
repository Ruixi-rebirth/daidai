package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

// FaceTool 呆呆专属形象照片与表情工具 (实现 core.Tool，优先读取本地缓存，未命中时参考基准形象无状态动态生成并缓存)
type FaceTool struct {
	delegates *entity.Delegates
	baseDir   string
}

// NewFaceTool 实例化形象与表情工具
func NewFaceTool(delegates *entity.Delegates) *FaceTool {
	return NewFaceToolWithDir(delegates, filepath.Join("data", "assets", "faces"))
}

// NewFaceToolWithDir 指定持久化目录实例化形象与表情工具
func NewFaceToolWithDir(delegates *entity.Delegates, baseDir string) *FaceTool {
	_ = os.MkdirAll(baseDir, 0755)
	return &FaceTool{
		delegates: delegates,
		baseDir:   baseDir,
	}
}

func (t *FaceTool) Name() string {
	return "express_face"
}

func (t *FaceTool) Description() string {
	return "发送呆呆本人的照片、自拍或表情贴纸（支持任意拍摄角度、动作或表情，如：正脸、侧脸、半身、全身、开心、害羞、得意、比心、委屈等；本地已有则直接发送，尚无则自动按呆呆本人设定现拍生成并缓存）。"
}

func (t *FaceTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"emotion": map[string]any{
				"type":        "string",
				"description": "呆呆的照片角度、动作或表情名称，例如：'正脸'、'侧脸'、'全身'、'开心'、'害羞'、'得意'、'比心'、'卖萌'、'委屈'、'思考'等",
			},
		},
		"required": []string{"emotion"},
	}
}

type faceArgs struct {
	Emotion string `json:"emotion"`
}

func normalizeEmotion(raw string) string {
	e := strings.TrimSpace(raw)
	e = strings.ReplaceAll(e, "/", "")
	e = strings.ReplaceAll(e, "\\", "")
	e = strings.ReplaceAll(e, "..", "")
	switch e {
	case "", "默认":
		return "开心"
	case "正脸照", "自拍", "自拍照", "照片", "本人", "形象", "日常":
		return "正脸"
	case "侧脸照", "侧脸图", "侧面", "侧颜":
		return "侧脸"
	default:
		return e
	}
}

func (t *FaceTool) readCachedFace(emotion string) ([]byte, bool) {
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp"} {
		p := filepath.Join(t.baseDir, emotion+ext)
		if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
			return data, true
		}
	}
	return nil, false
}

func (t *FaceTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	var args faceArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid express_face args: %w", err)
	}

	emotion := normalizeEmotion(args.Emotion)

	// 1. 检查本地缓存是否存在该形象或表情
	if data, ok := t.readCachedFace(emotion); ok {
		return &core.ToolResult{
			Text:      fmt.Sprintf("已发送呆呆的【%s】图片", emotion),
			ImageData: data,
		}, nil
	}

	event := entity.SensoryEventFromContext(ctx)
	if event != nil && event.Role == entity.RoleGuest {
		for _, fallback := range []string{"开心", "正脸"} {
			if data, ok := t.readCachedFace(fallback); ok {
				return &core.ToolResult{
					Text:      fmt.Sprintf("已发送呆呆的【%s】图片", fallback),
					ImageData: data,
				}, nil
			}
		}
		return &core.ToolResult{
			Text: fmt.Sprintf("本地暂无【%s】的现成照片缓存", emotion),
		}, nil
	}

	// 2. 本地缓存未命中，无状态调用 agy + image-generator 动态生成并写入缓存
	if t.delegates == nil {
		return &core.ToolResult{
			Text: fmt.Sprintf("暂无可用工单引擎动态生成【%s】图片", emotion),
		}, nil
	}

	if event != nil && event.OnProgress != nil {
		event.OnProgress("稍等，我现拍一张~")
	}

	cachePath := filepath.Join(t.baseDir, emotion+".jpg")
	absCachePath, absErr := filepath.Abs(cachePath)
	if absErr != nil {
		absCachePath = cachePath
	}

	refPath, _ := filepath.Abs(filepath.Join(t.baseDir, "正脸.jpg"))
	goal := fmt.Sprintf("请调用 image-generator 子智能体生成一张呆呆本人的二次元插画照片（主题/角度/神态：%s），请让 image-generator 先读取参考基准图 %s 以严格保持同一人物的五官、发色（黑茶色微卷中长发）、白色小兔发夹、蓝灰眼眸与画风一致。生成完成后，将图片文件复制到目标路径：%s 。", emotion, refPath, absCachePath)

	slog.Info("face_tool: 触发无状态后台动态生成呆呆新图片", "emotion", emotion)
	if _, execErr := runStatelessImageGen(ctx, goal, filepath.Dir(absCachePath)); execErr != nil {
		slog.Warn("face_tool: 动态生成图片失败", "emotion", emotion, "err", execErr)
		return &core.ToolResult{
			Text: fmt.Sprintf("动态生成【%s】图片失败：%v", emotion, execErr),
		}, nil
	}

	// 3. 读取生成后的图片
	if data, ok := t.readCachedFace(emotion); ok {
		return &core.ToolResult{
			Text:      fmt.Sprintf("已生成并发送呆呆的【%s】图片", emotion),
			ImageData: data,
		}, nil
	}

	return &core.ToolResult{
		Text: fmt.Sprintf("已尝试生成【%s】图片，但未找到输出文件", emotion),
	}, nil
}
