// Package recall implements semantic memory recall for the PM
// decision prompt. Given a "query" snippet (typically built from
// the day's MacroBriefing + universe symbols), it returns the top-k
// most-related historical memories by cosine similarity against
// the memories.embedding column populated by the embed worker.
//
// Sprint 3 / L3 motivation:
//
//   * recentLessons 现在是时间窗口召回 — "过去 14 天的"。语义召回
//     补一刀："和今天主题相关的"。两者并行，PM 可同时看到时间和
//     语义维度的过去。
//
//   * 我们不替换 recentLessons —— 那个有自己稳定的 hit-rate /
//     lineage feedback。recall 输出走独立的 prompt 字段
//     (semanticallyRecalledLessons)，PM 自行权衡。
//
// 设计取舍：
//
//  * 当数据库没装 pgvector / 没填充 embedding 时，Recall 返回 nil
//    而不是 error —— PM 在没有该信号时正常工作（兼容 dev / smoke）。
//  * 我们不在这里 embed query；query embed 是调用方传入。调用方在
//    无 embed provider 时跳过 recall.Query。
//  * 结果只回 ID + 相似度 score + 摘要 content，调用方负责把它转换
//    成 RecentLessonContext 喂入 prompt。
package recall

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoEmbedding 表示某条 query / memory 没有 embedding，无法做 cosine
// 比对。调用方静默跳过即可。
var ErrNoEmbedding = errors.New("recall: missing embedding vector")

// Result 是一行召回。Similarity ∈ [0, 1]，越大越相似（cosine 距离的
// 互补：1 - distance）。Snippet 是 memories.content 的前 N 字符。
type Result struct {
	MemoryID     string    `json:"memoryId"`
	FundID       string    `json:"fundId"`
	Layer        string    `json:"layer"`
	Title        string    `json:"title"`
	Snippet      string    `json:"snippet"`
	CreatedAt    time.Time `json:"createdAt"`
	Similarity   float64   `json:"similarity"`
	Tags         []string  `json:"tags,omitempty"`
}

// Service 持有 db handle。EmbeddingDimensions 应该与 migration 042 里
// vector(1536) 一致；后续切到别的 embed model 时同步改。
type Service struct {
	db                 *sql.DB
	EmbeddingDimension int
	// SimilarityFloor 是回结果的最低 cosine 相似度。0.55 是 OpenAI
	// text-embedding-3-small 的一个经验阈值 — 低于这个的几乎都是
	// 偶然命中。
	SimilarityFloor float64
}

// New 构造默认配置。
func New(db *sql.DB) *Service {
	return &Service{
		db:                 db,
		EmbeddingDimension: 1536,
		SimilarityFloor:    0.55,
	}
}

// Query 返回 top-k 最相似的、特定 fund / layer 的 memories。
// queryEmbedding 长度必须 = EmbeddingDimension；空切片 → ErrNoEmbedding。
// fundID 不限制（""）时跨基金召回 — 这只在 admin scope 下使用；
// 调用方常规传 fundID。
func (s *Service) Query(ctx context.Context, fundID string, layer string, queryEmbedding []float32, topK int) ([]Result, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if len(queryEmbedding) == 0 {
		return nil, ErrNoEmbedding
	}
	if len(queryEmbedding) != s.EmbeddingDimension {
		return nil, fmt.Errorf("recall: query embedding dim mismatch: got %d want %d", len(queryEmbedding), s.EmbeddingDimension)
	}
	if topK <= 0 {
		topK = 5
	}
	// pgvector 用 '<=>' 表示 cosine 距离 (1 - similarity)；
	// 我们在 SELECT 里反推 similarity = 1 - distance。
	// 用 prepared parameter 把 query embedding 作 pgvector literal 传进去。
	literal := vectorLiteral(queryEmbedding)

	args := []any{literal, topK}
	where := []string{"embedding IS NOT NULL"}
	if strings.TrimSpace(fundID) != "" {
		args = append(args, fundID)
		where = append(where, fmt.Sprintf("fund_id = $%d", len(args)))
	}
	if strings.TrimSpace(layer) != "" {
		args = append(args, layer)
		where = append(where, fmt.Sprintf("layer = $%d", len(args)))
	}
	// SimilarityFloor → distance ceiling: dist <= 1 - floor
	if s.SimilarityFloor > 0 {
		args = append(args, 1-s.SimilarityFloor)
		where = append(where, fmt.Sprintf("(embedding <=> $1) <= $%d", len(args)))
	}
	q := fmt.Sprintf(`
SELECT id, fund_id, layer, COALESCE(title, ''), content, tags, created_at,
       1 - (embedding <=> $1) AS similarity
  FROM memories
 WHERE %s
 ORDER BY embedding <=> $1
 LIMIT $2`, strings.Join(where, " AND "))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("recall: query: %w", err)
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		var content string
		var tags []string
		ts := tagsScannerFor(&tags)
		if err := rows.Scan(&r.MemoryID, &r.FundID, &r.Layer, &r.Title, &content, &ts, &r.CreatedAt, &r.Similarity); err != nil {
			return nil, err
		}
		r.Snippet = snippet(content, 220)
		r.Tags = tags
		out = append(out, r)
	}
	return out, rows.Err()
}

// vectorLiteral 把 float32 切片转成 pgvector "[1,2,3]" 字面量。
// pgvector 的 driver 在 jackc/pgx 下有 native 支持，但 lib/pq 没有，
// 字符串方式是最大兼容写法。
func vectorLiteral(v []float32) string {
	var sb strings.Builder
	sb.Grow(len(v) * 8)
	sb.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(formatFloat32(x))
	}
	sb.WriteByte(']')
	return sb.String()
}

func formatFloat32(x float32) string {
	// 6 位小数对 cosine 相似度精度足够；更多位会让 query 字符串变得很大。
	return fmt.Sprintf("%.6f", x)
}

func snippet(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max])) + "…"
}

// tagsScannerFor 把 lib/pq 的 text[] 列扫到 []string slice。
// pgx 自动支持 array；这里我们用一个简单适配器，让 lib/pq 也能用。
// 如果 driver 不支持，列扫描会报错；调用方需要 fallback。
type tagsScanner struct{ dst *[]string }

func tagsScannerFor(dst *[]string) tagsScanner { return tagsScanner{dst: dst} }

func (t tagsScanner) Scan(src any) error {
	if src == nil {
		*t.dst = nil
		return nil
	}
	// lib/pq returns text[] as []byte like "{a,b,c}". pgx returns []any.
	switch v := src.(type) {
	case []byte:
		raw := string(v)
		if len(raw) < 2 {
			return nil
		}
		raw = strings.TrimSpace(raw)
		raw = strings.TrimPrefix(raw, "{")
		raw = strings.TrimSuffix(raw, "}")
		if raw == "" {
			return nil
		}
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(strings.Trim(p, "\""))
			if p != "" {
				out = append(out, p)
			}
		}
		*t.dst = out
	case string:
		// pgx string form: "{a,b}"
		return tagsScannerFor(t.dst).Scan([]byte(v))
	}
	return nil
}
