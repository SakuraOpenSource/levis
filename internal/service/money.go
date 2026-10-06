package service

import "math/big"

// mulDivCents floors a nonnegative ratio without intermediate overflow.
func mulDivCents(amount, numerator, denominator int64) (int64, error) {
	if amount < 0 || numerator < 0 || denominator <= 0 {
		return 0, ErrBadRequest("金额或比例无效")
	}
	out := new(big.Int).Mul(big.NewInt(amount), big.NewInt(numerator))
	out.Quo(out, big.NewInt(denominator))
	if !out.IsInt64() {
		return 0, ErrBadRequest("金额过大")
	}
	return out.Int64(), nil
}
