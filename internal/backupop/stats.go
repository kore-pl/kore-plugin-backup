package backupop

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// callStats 处理 backup.stats：汇总总数、总字节、最早/最晚创建时间与按 name 分组。
func callStats(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p struct{}
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	backups, err := exec.List(opts.Dir)
	if err != nil {
		return nil, classify(err)
	}
	result := StatsData{ByName: []StatsNameBucket{}}
	buckets := make(map[string]StatsNameBucket, len(backups))
	var oldest, newest string
	var oldestAt, newestAt time.Time
	for _, backup := range backups {
		result.TotalCount++
		result.TotalBytes += backup.SizeBytes
		bucket := buckets[backup.Name]
		bucket.Name = backup.Name
		bucket.Count++
		bucket.Bytes += backup.SizeBytes
		buckets[backup.Name] = bucket
		// 创建时间无法解析的条目不参与最早/最晚判定，但照常计入总数与分组。
		created := parsedTime(backup.CreatedAt)
		if created.IsZero() {
			continue
		}
		if oldest == "" || created.Before(oldestAt) {
			oldest, oldestAt = backup.CreatedAt, created
		}
		if newest == "" || created.After(newestAt) {
			newest, newestAt = backup.CreatedAt, created
		}
	}
	if oldest != "" {
		result.Oldest = &oldest
	}
	if newest != "" {
		result.Newest = &newest
	}
	for _, bucket := range buckets {
		result.ByName = append(result.ByName, bucket)
	}
	sort.Slice(result.ByName, func(i, j int) bool {
		if result.ByName[i].Count != result.ByName[j].Count {
			return result.ByName[i].Count > result.ByName[j].Count
		}
		return result.ByName[i].Name < result.ByName[j].Name
	})
	return encode(result)
}
