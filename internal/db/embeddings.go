package db

import (
	"context"
	"encoding/binary"
	"math"
	"strings"
)

func encodeFloat32Slice(vec []float32) []byte {
	if len(vec) == 0 {
		return nil
	}
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func decodeFloat32Slice(blob []byte) []float32 {
	if len(blob) == 0 || len(blob)%4 != 0 {
		return nil
	}
	n := len(blob) / 4
	vec := make([]float32, n)
	for i := 0; i < n; i++ {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return vec
}

// UpdateMessageEmbedding 异步回填消息的稠密语义向量
func (m *Manager) UpdateMessageEmbedding(ctx context.Context, platform string, msgID int64, vec []float32, modelIdentity string) error {
	if msgID <= 0 || len(vec) == 0 || strings.TrimSpace(modelIdentity) == "" {
		return nil
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "UPDATE messages SET embedding = ?, embedding_model = ? WHERE id = ?", encodeFloat32Slice(vec), modelIdentity, msgID)
	return err
}

// UpdateFactEmbedding 异步回填长期事实的稠密语义向量
func (m *Manager) UpdateFactEmbedding(ctx context.Context, platform string, factID int64, expectedFact string, vec []float32, modelIdentity string) error {
	if factID <= 0 || len(vec) == 0 || strings.TrimSpace(modelIdentity) == "" {
		return nil
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "UPDATE user_facts SET embedding = ?, embedding_model = ? WHERE id = ? AND fact = ? AND status = 'active' AND (embedding IS NULL OR length(embedding) = 0 OR embedding_model <> ?)", encodeFloat32Slice(vec), modelIdentity, factID, expectedFact, modelIdentity)
	return err
}
