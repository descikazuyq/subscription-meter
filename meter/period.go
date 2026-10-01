package meter

import "time"

// Period 表示一个 UTC 自然月账期。
type Period struct {
	Year  int
	Month time.Month
}

// periodOf 返回 t 所在的 UTC 自然月账期。
func periodOf(t time.Time) Period {
	t = t.UTC()
	return Period{Year: t.Year(), Month: t.Month()}
}

// periodEnd 返回账期结束时刻，即下月月初（UTC）。
func periodEnd(p Period) time.Time {
	return time.Date(p.Year, p.Month+1, 1, 0, 0, 0, 0, time.UTC)
}

// periodCompare 按先后顺序比较两个账期：-1 表示 a 早于 b，0 表示相同，1 表示晚于。
func periodCompare(a, b Period) int {
	if a.Year != b.Year {
		if a.Year < b.Year {
			return -1
		}
		return 1
	}
	if a.Month != b.Month {
		if a.Month < b.Month {
			return -1
		}
		return 1
	}
	return 0
}
