package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"daidai/assets"
	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

// FaceTool 呆呆专属形象照片与表情工具 (实现 core.Tool，优先读取本地缓存，未命中时参考基准形象无状态动态生成并缓存)
type FaceTool struct {
	delegates *entity.Delegates
	immune    *entity.ImmuneSystem
	router    *llm.MindRouter
	command   string
	baseDir   string
}

// SetRouter binds the mind router so stateless selfie/face generation can prefer lightweight coordinator models.
func (t *FaceTool) SetRouter(router *llm.MindRouter) {
	if t != nil {
		t.router = router
	}
}

// NewFaceTool 实例化形象与表情工具
func NewFaceTool(delegates *entity.Delegates, immune ...*entity.ImmuneSystem) *FaceTool {
	return NewFaceToolWithCommand(delegates, "agy", filepath.Join("data", "assets", "faces"), immune...)
}

// NewFaceToolWithDir 指定持久化目录实例化形象与表情工具
func NewFaceToolWithDir(delegates *entity.Delegates, baseDir string, immune ...*entity.ImmuneSystem) *FaceTool {
	return NewFaceToolWithCommand(delegates, "agy", baseDir, immune...)
}

func NewFaceToolWithCommand(delegates *entity.Delegates, command, baseDir string, immune ...*entity.ImmuneSystem) *FaceTool {
	command = strings.TrimSpace(command)
	if command == "" {
		command = "agy"
	}
	var immuneSystem *entity.ImmuneSystem
	if len(immune) > 0 {
		immuneSystem = immune[0]
	}
	return &FaceTool{
		delegates: delegates,
		immune:    immuneSystem,
		command:   command,
		baseDir:   baseDir,
	}
}

func (t *FaceTool) Name() string {
	return "express_face"
}

func (t *FaceTool) Description() string {
	return fmt.Sprintf("仅当用户明确要求看%s本人的自拍、正脸、全身照或特定换装造型时调用（日常聊天带小表情请直接在回复末尾写 [脸:X]，不必调用本工具）。优先读取本地缓存；核心搭档和协作者可在未命中时现拍生成，访客仅获取本地缓存或内置表情。", config.BotName())
}

func (t *FaceTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"emotion": map[string]any{
				"type":        "string",
				"description": fmt.Sprintf("%s的照片角度、动作或表情名称，例如：'正脸'、'侧脸'、'全身'、'开心'、'害羞'、'得意'、'比心'、'卖萌'、'委屈'、'思考'等", config.BotName()),
			},
		},
		"required": []string{"emotion"},
	}
}

type faceArgs struct {
	Emotion string `json:"emotion"`
}

func preparedFaceImage(data []byte) *core.ToolResult {
	return &core.ToolResult{
		ImageData:         data,
		ImageOnly:         true,
		FixedPresentation: true,
	}
}

func normalizeEmotion(raw string) string {
	e := strings.TrimSpace(raw)
	e = strings.ReplaceAll(e, "/", "")
	e = strings.ReplaceAll(e, "\\", "")
	e = strings.ReplaceAll(e, "..", "")
	e = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, e)
	if runes := []rune(e); len(runes) > 80 {
		e = string(runes[:80])
	}
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

func (t *FaceTool) readCachedFace(ctx context.Context, emotion string) ([]byte, bool) {
	entries, err := os.ReadDir(t.baseDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() || strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())) != emotion {
				continue
			}
			data, err := readFileLimited(filepath.Join(t.baseDir, entry.Name()), maxGeneratedImageBytes)
			if err != nil {
				continue
			}
			normalized, _, err := normalizeImageData(ctx, data)
			if err == nil {
				return normalized, true
			}
		}
	}
	data, err := assets.SmallFaces.ReadFile(filepath.Join("daidai_faces_small", emotion+".png"))
	if err == nil {
		if normalized, _, normalizeErr := normalizeImageData(ctx, data); normalizeErr == nil {
			return normalized, true
		}
	}
	return nil, false
}

func (t *FaceTool) referenceFacePath(ctx context.Context, dir string) (string, func(), error) {
	if data, ok := t.readCachedFace(ctx, "正脸"); ok {
		return t.writeReferenceImage(data, dir)
	}

	data, err := assets.SmallFaces.ReadFile(filepath.Join("daidai_faces_small", "开心.png"))
	if err != nil {
		return "", nil, fmt.Errorf("读取内置表情参考图失败: %w", err)
	}
	normalized, _, err := normalizeImageData(ctx, data)
	if err != nil {
		return "", nil, fmt.Errorf("解码内置表情参考图失败: %w", err)
	}
	return t.writeReferenceImage(normalized, dir)
}

func (t *FaceTool) writeReferenceImage(data []byte, dir string) (string, func(), error) {
	tmpFile, err := os.CreateTemp(dir, "face-reference-*.png")
	if err != nil {
		return "", nil, fmt.Errorf("创建临时参考图失败: %w", err)
	}
	path := tmpFile.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return "", nil, fmt.Errorf("写入临时参考图失败: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("关闭临时参考图失败: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("解析临时参考图路径失败: %w", err)
	}
	return absPath, cleanup, nil
}

func (t *FaceTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	var args faceArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid express_face args: %w", err)
	}

	emotion := normalizeEmotion(args.Emotion)
	if t.delegates != nil {
		releaseWorkspace, err := t.delegates.AcquireWorkspaceLease(t.baseDir)
		if err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("读取形象缓存失败：%v", err)}, nil
		}
		defer releaseWorkspace()
	}

	// 1. 检查本地缓存是否存在该形象或表情
	if data, ok := t.readCachedFace(ctx, emotion); ok {
		return preparedFaceImage(data), nil
	}

	event := entity.SensoryEventFromContext(ctx)
	if event == nil {
		if err := llm.CheckAGYAuth(); err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("动态生成【%s】图片失败：%v", emotion, err)}, nil
		}
		return &core.ToolResult{Text: "无法校验当前会话权限，不能生成新图片。"}, nil
	}
	if event.Role != entity.RolePartner && event.Role != entity.RoleCollaborator {
		for _, fallback := range []string{"开心", "正脸"} {
			if data, ok := t.readCachedFace(ctx, fallback); ok {
				return preparedFaceImage(data), nil
			}
		}
		return &core.ToolResult{
			Text: fmt.Sprintf("本地暂无【%s】的现成照片缓存", emotion),
		}, nil
	}
	if t.immune == nil {
		return &core.ToolResult{Text: "权限校验未配置，拒绝动态生成形象图片。"}, nil
	}
	role := t.immune.ResolveRole(event.SenderID)
	if err := t.immune.VerifyFaceGenPerm(role); err != nil {
		return &core.ToolResult{Text: err.Error()}, nil
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

	if err := os.MkdirAll(t.baseDir, 0755); err != nil {
		return &core.ToolResult{Text: fmt.Sprintf("创建形象缓存目录失败：%v", err)}, nil
	}
	workDir, err := os.MkdirTemp(t.baseDir, "face-gen-")
	if err != nil {
		return &core.ToolResult{Text: fmt.Sprintf("创建形象临时工作区失败：%v", err)}, nil
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	if err := os.Chmod(workDir, 0700); err != nil {
		return &core.ToolResult{Text: fmt.Sprintf("保护形象临时工作区失败：%v", err)}, nil
	}
	tmpPath := filepath.Join(workDir, "daidai.png")
	absTmpPath, err := filepath.Abs(tmpPath)
	if err != nil {
		return &core.ToolResult{Text: fmt.Sprintf("解析形象输出路径失败：%v", err)}, nil
	}

	refPath, cleanupRef, err := t.referenceFacePath(ctx, workDir)
	if err != nil {
		return &core.ToolResult{Text: fmt.Sprintf("准备形象参考图失败：%v", err)}, nil
	}
	defer cleanupRef()
	goal := fmt.Sprintf(
		"请直接调用 generate_image 工具或 image-generator 子智能体生成一张%s本人的 2D 二次元平面赛璐璐插画（主题/角度/神态：%s；绝对不要真人照片、3D CG 或可读文字水印）。请以参考基准图 %s 作为视觉参考（如需查看仅可使用 view_file），保持同一人物的五官、发色（黑茶色微卷中长发）、白色小兔发夹、蓝灰眼眸与温柔文艺的 INFP 气质一致。%s 目标输出路径为 %s（若工具仅保存到会话产物目录也可）。重要：严禁调用 run_command 或任何终端命令（无头沙箱模式下调用命令会立刻中止任务）；生成完成后无需用命令检查、移动或转换格式，直接结束任务即可。",
		config.BotName(), emotion, refPath, faceSceneAtmosphere(time.Now()), absTmpPath,
	)

	slog.Info("face_tool: 触发无状态后台动态生成新图片", "bot", config.BotName(), "emotion", emotion)
	agyResult, artifactHome, cleanupAGY, execErr := runStatelessImageGen(ctx, t.command, goal, workDir, t.router)
	if cleanupAGY != nil {
		defer cleanupAGY()
	}
	if execErr != nil {
		slog.Warn("face_tool: 动态生成图片失败", "emotion", emotion, "err", execErr)
		return &core.ToolResult{
			Text: fmt.Sprintf("动态生成【%s】图片失败：%v", emotion, execErr),
		}, nil
	}

	data, generatedPath, err := readGeneratedImage(ctx, tmpPath, workDir, refPath)
	if os.IsNotExist(err) {
		data, generatedPath, err = readAGYImageArtifact(ctx, agyResult.ConversationID, artifactHome)
	}
	if err != nil {
		slog.Warn("face_tool: 生成结果读取失败", "emotion", emotion, "err", err, "conversationID", agyResult.ConversationID, "outputBytes", len(agyResult.Response), "deniedActions", agyResult.DeniedActionSummary())
		if os.IsNotExist(err) {
			return &core.ToolResult{Text: fmt.Sprintf("生成任务已结束，但没有找到【%s】图片文件；未确认失败原因，不要推测为超时或上游服务故障。", emotion)}, nil
		}
		return &core.ToolResult{Text: fmt.Sprintf("已生成【%s】图片，但读取失败；请检查服务日志。", emotion)}, nil
	}
	if generatedPath != tmpPath {
		slog.Info("face_tool: 从生成器实际输出文件中恢复图片", "emotion", emotion, "file", filepath.Base(generatedPath))
	}
	if len(data) == 0 {
		return &core.ToolResult{Text: fmt.Sprintf("已尝试生成【%s】图片，但输出文件为空", emotion)}, nil
	}
	normalized, format, normalizeErr := normalizeImageData(ctx, data)
	if normalizeErr != nil {
		slog.Warn("face_tool: 生成结果无法解码或转换", "emotion", emotion, "contentType", imageContentType(data), "format", format, "bytes", len(data), "err", normalizeErr)
		return &core.ToolResult{Text: fmt.Sprintf("生成的【%s】图片无法识别或转换（检测类型：%s）：%v", emotion, imageContentType(data), normalizeErr)}, nil
	}
	cacheFile, err := os.CreateTemp(t.baseDir, ".face-cache-*.png")
	if err != nil {
		slog.Warn("face_tool: 创建规范化缓存临时文件失败", "emotion", emotion, "err", err)
		return &core.ToolResult{Text: fmt.Sprintf("已生成【%s】图片，但暂时无法保存到形象缓存；请重试。", emotion)}, nil
	}
	cacheTempPath := cacheFile.Name()
	defer func() { _ = os.Remove(cacheTempPath) }()
	if _, err := cacheFile.Write(normalized); err != nil {
		_ = cacheFile.Close()
		slog.Warn("face_tool: 写入规范化缓存失败", "emotion", emotion, "err", err)
		return &core.ToolResult{Text: fmt.Sprintf("已生成【%s】图片，但暂时无法保存到形象缓存；请重试。", emotion)}, nil
	}
	if err := cacheFile.Close(); err != nil {
		slog.Warn("face_tool: 关闭规范化缓存失败", "emotion", emotion, "err", err)
		return &core.ToolResult{Text: fmt.Sprintf("已生成【%s】图片，但暂时无法保存到形象缓存；请重试。", emotion)}, nil
	}
	cachePath := filepath.Join(t.baseDir, emotion+".png")
	if err := os.Rename(cacheTempPath, cachePath); err != nil {
		slog.Warn("face_tool: 更新形象缓存失败", "emotion", emotion, "err", err)
		return &core.ToolResult{Text: fmt.Sprintf("已生成【%s】图片，但暂时无法保存到形象缓存；请重试。", emotion)}, nil
	}
	return preparedFaceImage(normalized), nil
}

var faceSceneZone = time.FixedZone("CST", 8*3600)

func faceSceneAtmosphere(now time.Time) string {
	local := now.In(faceSceneZone)
	var season string
	switch local.Month() {
	case time.March, time.April, time.May:
		season = "春日清透窗边与柔和浅色针织日常穿搭"
	case time.June, time.July, time.August:
		season = "夏日清爽薄衬衫与绿植薄纱窗景"
	case time.September, time.October, time.November:
		season = "秋日温暖软针织与暖调木质房间氛围"
	default:
		season = "冬日温暖针织毛衣与温馨室内氛围"
	}
	var light string
	switch h := local.Hour(); {
	case h >= 0 && h < 6:
		light = "深夜暖黄小夜灯/台灯柔光，窗外安静夜色"
	case h >= 6 && h < 8:
		light = "清晨淡蓝白透明晨光"
	case h >= 8 && h < 16:
		light = "白天自然通透的明亮窗光"
	case h >= 16 && h < 18:
		light = "傍晚橘金色柔和逆光"
	case h >= 18 && h < 22:
		light = "夜晚窗外夜景与室内暖黄台灯光晕"
	default:
		light = "深夜安静暗调与小台灯暖光"
	}
	return fmt.Sprintf("若主题未指定特殊服装或背景，环境参考当前季节与真实时段（%s；%s），身边可自然有一只治愈系小猫或书桌小物。", season, light)
}
