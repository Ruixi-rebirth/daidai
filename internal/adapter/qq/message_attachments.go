package qq

import (
	"context"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"

	"daidai/internal/core"
)

type qqInboundMedia struct {
	Content       string
	Images        []*core.Attachment
	OmittedImages int
	Files         []*core.Attachment
	OmittedFiles  int
}

func (q *QQChannel) extractInboundMedia(ctx context.Context, d map[string]any, msgID, content string) qqInboundMedia {
	var images []*core.Attachment
	var omittedImages int
	var files []*core.Attachment
	var omittedFiles int

	// 只有明确标记为图片的附件进入视觉模型；类型不明的附件按普通文件处理，须用户明确要求后才读取。
	if attList, ok := d["attachments"].([]any); ok {
		for _, item := range attList {
			if attMap, ok := item.(map[string]any); ok {
				fileURL, _ := attMap["url"].(string)
				contentType, _ := attMap["content_type"].(string)
				fileName, _ := attMap["filename"].(string)
				if fileName == "" {
					fileName, _ = attMap["name"].(string)
				}
				fileName = core.TruncateRunes(strings.TrimSpace(fileName), 160, "…")
				contentType = strings.ToLower(strings.TrimSpace(core.TruncateRunes(contentType, 128, "")))
				baseContentType := strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
				if fileName != "" && (baseContentType == "" || baseContentType == "application/octet-stream" || baseContentType == "binary/octet-stream") {
					guessedType := strings.ToLower(strings.TrimSpace(strings.SplitN(mime.TypeByExtension(filepath.Ext(fileName)), ";", 2)[0]))
					if guessedType != "" {
						baseContentType = guessedType
					}
				}
				contentType = core.TruncateRunes(baseContentType, 128, "…")

				// 语音消息优先提取官方 ASR 转录文本或兜底转录，转为文本处理且不作为文件附件
				asrReferText, _ := attMap["asr_refer_text"].(string)
				asrReferText = strings.TrimSpace(asrReferText)
				voiceWavURL, _ := attMap["voice_wav_url"].(string)
				ext := strings.ToLower(filepath.Ext(fileName))

				isVoice := contentType == "voice" ||
					strings.HasPrefix(contentType, "voice") ||
					contentType == "audio/amr" ||
					contentType == "audio/silk" ||
					contentType == "audio/slk" ||
					voiceWavURL != "" ||
					asrReferText != "" ||
					ext == ".slk" || ext == ".silk" || ext == ".amr"

				if isVoice {
					audioURL := voiceWavURL
					if audioURL == "" {
						audioURL = fileURL
					}
					var recognizedText string
					currentMode := q.GetVoiceMode()
					if currentMode == VoiceModeFast {
						// 已有官方转录时直接使用，避免重复下载和转录。
						if asrReferText != "" {
							slog.Info("qq: 语音消息使用极速模式 ASR 转录", "msgID", msgID, "textRunes", len([]rune(asrReferText)))
							recognizedText = asrReferText
						} else if audioURL != "" {
							recognizedText = q.transcribeVoiceAudio(ctx, audioURL, fileName)
							if recognizedText != "" {
								slog.Info("qq: 极速模式 ASR 未命中，多模态原生转录成功", "msgID", msgID, "textRunes", len([]rune(recognizedText)))
							}
						}
					} else {
						// 优先转录原音；失败时仍可使用 QQ 给出的文本。
						if audioURL != "" {
							recognizedText = q.transcribeVoiceAudio(ctx, audioURL, fileName)
							if recognizedText != "" {
								slog.Info("qq: 语音消息多模态原生转录成功", "msgID", msgID, "textRunes", len([]rune(recognizedText)))
							}
						}
						// 若多模态转录未产生结果（如临时网络超时），回落至官方 ASR 文本兜底
						if recognizedText == "" && asrReferText != "" {
							slog.Info("qq: 多模态转录未命中，采用官方 ASR 文本兜底", "msgID", msgID, "textRunes", len([]rune(asrReferText)))
							recognizedText = asrReferText
						}
					}
					if recognizedText != "" {
						if content == "" {
							content = recognizedText
						} else {
							content = content + "\n" + recognizedText
						}
					} else {
						slog.Warn("qq: 语音消息未能解析出文字内容", "msgID", msgID)
						if content == "" {
							content = "【收到一段语音消息，但未能识别出文字内容】"
						}
					}
					// 语音消息已提取为文本，绝不作为普通文件附件推入 files，避免误触发附件处理拦截
					continue
				}

				if fileURL == "" {
					continue
				}
				if len(fileURL) > core.MaxAttachmentURLBytes {
					if strings.HasPrefix(contentType, "image/") {
						omittedImages++
					} else {
						omittedFiles++
					}
					continue
				}
				if strings.HasPrefix(contentType, "image/") {
					if len(images) >= core.MaxVisionImagesPerRequest {
						omittedImages++
						continue
					}
					images = append(images, &core.Attachment{Kind: core.AttachmentImage, URL: fileURL, MimeType: contentType, Name: fileName, SourceMessageID: msgID})
					continue
				}
				if len(files) >= core.MaxIncomingFilesPerRequest {
					omittedFiles++
					continue
				}
				files = append(files, &core.Attachment{Kind: core.AttachmentFile, URL: fileURL, MimeType: contentType, Name: fileName, SourceMessageID: msgID})
			}
		}
	}
	return qqInboundMedia{Content: content, Images: images, OmittedImages: omittedImages, Files: files, OmittedFiles: omittedFiles}
}
