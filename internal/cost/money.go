package cost

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	// microsPerUSD is the number of integer micro-dollars in one USD.
	microsPerUSD int64 = 1_000_000

	// MaxMicros ($1,000,000,000.00) bounds every accepted amount so sums of many
	// events cannot overflow int64.
	MaxMicros int64 = 1_000_000_000_000_000

	// maxUSD is the floating-point ceiling corresponding to MaxMicros.
	maxUSD float64 = 1_000_000_000.0
)

var (
	ErrMoneyRange    = errors.New("amount out of range")
	errNegativeMoney = errors.New("negative amount")
	errInvalidMoney  = errors.New("invalid amount")
)

// ParseMicros parses a non-negative decimal USD amount (plain, or with an
// exponent as JSON allows) exactly. It never goes through float64.
func ParseMicros(text string) (int64, error) {
	s := strings.TrimSpace(text)
	negative := strings.HasPrefix(s, "-")
	if negative {
		s = s[1:]
	}
	exp := 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e > 400 || e < -400 {
			return 0, ErrMoneyRange
		}
		exp, s = e, s[:i]
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		return 0, errInvalidMoney
	}
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return 0, errInvalidMoney
		}
	}
	digits := strings.TrimLeft(whole+frac, "0")
	if digits == "" {
		return 0, nil // "-0" is zero, not negative
	}
	if negative {
		return 0, errNegativeMoney
	}
	// value = digits * 10^(exp - len(frac)); micros = value * 10^6.
	shift := exp - len(frac) + 6
	if shift >= 0 {
		if len(digits)+shift > 19 {
			return 0, ErrMoneyRange
		}
		n, err := strconv.ParseInt(digits+strings.Repeat("0", shift), 10, 64)
		if err != nil || n > MaxMicros {
			return 0, ErrMoneyRange
		}
		return n, nil
	}
	drop := -shift
	if drop > len(digits) {
		return 0, nil // below half a micro
	}
	keep, rest := digits[:len(digits)-drop], digits[len(digits)-drop:]
	var n int64
	if keep != "" {
		var err error
		if n, err = strconv.ParseInt(keep, 10, 64); err != nil {
			return 0, ErrMoneyRange
		}
	}
	// Half to even on the dropped digits.
	half := "5" + strings.Repeat("0", drop-1)
	switch {
	case rest > half:
		n++
	case rest == half && n%2 == 1:
		n++
	}
	if n > MaxMicros {
		return 0, ErrMoneyRange
	}
	return n, nil
}

// MicrosFromFloat converts a float USD amount to integer micro-dollars.
// Rejects NaN, +/-Inf, negatives, and values exceeding MaxUSD ($1e9).
func MicrosFromFloat(f float64) (int64, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > maxUSD {
		return 0, false
	}
	m, err := ParseMicros(strconv.FormatFloat(f, 'f', -1, 64))
	return m, err == nil
}

// MicrosToFloat converts integer micro-dollars to float USD.
func MicrosToFloat(m int64) float64 {
	return float64(m) / float64(microsPerUSD)
}

// FormatMicros renders micro-dollars as a fixed six-decimal USD string.
func FormatMicros(m int64) string {
	sign := ""
	if m < 0 {
		sign, m = "-", -m
	}
	frac := strconv.FormatInt(m%microsPerUSD, 10)
	return sign + strconv.FormatInt(m/microsPerUSD, 10) + "." + strings.Repeat("0", 6-len(frac)) + frac
}

// FormatMicrosTrim renders micro-dollars without trailing zeros, suitable for CLI flags.
func FormatMicrosTrim(m int64) string {
	s := strings.TrimRight(FormatMicros(m), "0")
	return strings.TrimSuffix(s, ".")
}

// Add adds two micro-dollar amounts without wrapping.
func Add(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > MaxMicros || b > MaxMicros {
		return 0, false
	}
	sum := a + b
	if sum > MaxMicros || sum < a {
		return 0, false
	}
	return sum, true
}

// Limit converts a configured float budget limit to micro-dollars.
// A finite ceiling above the supported range is effectively unlimited (MaxInt64).
func Limit(f float64) (int64, bool) {
	if m, ok := MicrosFromFloat(f); ok {
		return m, true
	}
	if f > 0 && !math.IsInf(f, 0) {
		return math.MaxInt64, true
	}
	return 0, false
}

// CostExceeds reports whether cost exceeds budget.
func CostExceeds(cost, budget float64) bool {
	c, cok := MicrosFromFloat(cost)
	b, bok := Limit(budget)
	return !cok || !bok || c > b
}

// ResolveCost chooses the authoritative cost in micros between cost_micros and cost_usd.
// cost_micros takes precedence when non-zero; otherwise cost_usd is converted safely.
func ResolveCost(micros int64, usd float64) (int64, error) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0, fmt.Errorf("%w: invalid USD cost %v", errInvalidMoney, usd)
	}
	if micros > 0 {
		if micros > MaxMicros {
			return 0, fmt.Errorf("%w: %d exceeds maximum allowed %d", ErrMoneyRange, micros, MaxMicros)
		}
		return micros, nil
	}
	if micros < 0 {
		return 0, fmt.Errorf("%w: negative micros %d", errNegativeMoney, micros)
	}
	if usd < 0 {
		return 0, fmt.Errorf("%w: negative USD %v", errNegativeMoney, usd)
	}
	if usd > 0 {
		m, ok := MicrosFromFloat(usd)
		if !ok {
			return 0, fmt.Errorf("%w: invalid USD cost %v", errInvalidMoney, usd)
		}
		return m, nil
	}
	return 0, nil
}

// ResolveCostExplicit is ResolveCost for boundaries that know whether
// cost_micros was actually present on the wire. A present cost_micros is
// authoritative, including an explicit zero, and is never replaced by cost_usd.
// When it is absent the sentinel rule applies: zero micros means
// "not supplied" and cost_usd is converted. Every invalid input fails closed,
// including a negative, non-finite or out-of-range cost_usd that accompanies an
// explicit cost_micros; such a conflict is evidence of a corrupt response and
// must not be resolved to a smaller charge.
func ResolveCostExplicit(micros int64, microsPresent bool, usd float64) (int64, error) {
	if !microsPresent {
		return ResolveCost(micros, usd)
	}
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0, fmt.Errorf("%w: invalid USD cost %v", errInvalidMoney, usd)
	}
	if usd < 0 {
		return 0, fmt.Errorf("%w: negative USD %v", errNegativeMoney, usd)
	}
	if usd > maxUSD {
		return 0, fmt.Errorf("%w: USD cost %v exceeds maximum allowed %v", ErrMoneyRange, usd, maxUSD)
	}
	if micros < 0 {
		return 0, fmt.Errorf("%w: negative micros %d", errNegativeMoney, micros)
	}
	if micros > MaxMicros {
		return 0, fmt.Errorf("%w: %d exceeds maximum allowed %d", ErrMoneyRange, micros, MaxMicros)
	}
	return micros, nil
}
