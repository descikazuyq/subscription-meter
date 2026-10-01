package meter

import (
	"math"
	"math/big"
	"math/bits"
)

// add64 做带溢出检查的 int64 加法。
func add64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// mul64 做带溢出检查的 int64 乘法。
func mul64(a, b int64) (int64, bool) {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 {
		return 0, false
	}
	return int64(lo), true
}

// taxCents 对 base（分）按万分比税率计税，四舍五入到分。
// base 非负、rateBP 在 0..10000 时结果必然不超过 base，仍做溢出检查。
func taxCents(base int64, rateBP int64) (int64, bool) {
	b := big.NewInt(base)
	r := big.NewInt(rateBP)
	num := new(big.Int).Mul(b, r)
	num.Add(num, big.NewInt(5000)) // 四舍五入：先加半档
	q := new(big.Int).Quo(num, big.NewInt(10000))
	if !q.IsInt64() {
		return 0, false
	}
	return q.Int64(), true
}
