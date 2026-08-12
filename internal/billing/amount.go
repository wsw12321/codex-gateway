// Package billing implements exact decimal arithmetic for gateway billing.
// Monetary values never pass through float64.
package billing

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

const (
	AmountScale      = 12
	InputScale       = 6
	RateScale        = 12
	maxIntegerDigits = 18
)

var (
	ErrInvalidDecimal = errors.New("billing: invalid decimal")
	decimalPattern    = regexp.MustCompile(`^(0|[1-9][0-9]*)(?:\.([0-9]+))?$`)
)

// ParseInput validates a recharge, adjustment, or subscription amount. Inputs
// may carry at most six fractional digits. The returned value is canonical but
// is not padded with trailing zeroes.
func ParseInput(value string, signed, allowZero bool) (string, error) {
	return parseBounded(value, InputScale, signed, allowZero)
}

// ParseRate validates a positive recharge conversion rate with at most twelve
// fractional digits.
func ParseRate(value string) (string, error) {
	return parseBounded(value, RateScale, false, false)
}

// ParsePrice validates a non-negative per-million-token price that can be
// stored exactly in a NUMERIC(30,12) snapshot column.
func ParsePrice(value string) (string, error) {
	return parseBounded(value, AmountScale, false, true)
}

func parseBounded(value string, scale int, signed, allowZero bool) (string, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return "", ErrInvalidDecimal
	}
	negative := false
	unsigned := value
	if strings.HasPrefix(unsigned, "-") {
		if !signed {
			return "", ErrInvalidDecimal
		}
		negative = true
		unsigned = strings.TrimPrefix(unsigned, "-")
	} else if strings.HasPrefix(unsigned, "+") {
		if !signed {
			return "", ErrInvalidDecimal
		}
		unsigned = strings.TrimPrefix(unsigned, "+")
	}
	matches := decimalPattern.FindStringSubmatch(unsigned)
	if matches == nil || len(matches[1]) > maxIntegerDigits || len(matches[2]) > scale {
		return "", ErrInvalidDecimal
	}
	integer := strings.TrimLeft(matches[1], "0")
	if integer == "" {
		integer = "0"
	}
	fraction := strings.TrimRight(matches[2], "0")
	canonical := integer
	if fraction != "" {
		canonical += "." + fraction
	}
	if canonical == "0" {
		if !allowZero {
			return "", ErrInvalidDecimal
		}
		return canonical, nil
	}
	if negative {
		canonical = "-" + canonical
	}
	return canonical, nil
}

// MultiplyToAmount returns left*right rounded half away from zero to twelve
// decimal places. It is used for recharge conversion snapshots.
func MultiplyToAmount(left, right string) (string, error) {
	left, err := ParseInput(left, false, false)
	if err != nil {
		return "", fmt.Errorf("invalid amount: %w", err)
	}
	right, err = ParseRate(right)
	if err != nil {
		return "", fmt.Errorf("invalid rate: %w", err)
	}
	l, _ := new(big.Rat).SetString(left)
	r, _ := new(big.Rat).SetString(right)
	return roundAmount(new(big.Rat).Mul(l, r))
}

// CalculateCost applies the requested-model price snapshot. Cached input is a
// subset of input, and reasoning tokens are already included in output.
func CalculateCost(inputTokens, cachedInputTokens, outputTokens int64, inputPrice, cachedPrice, outputPrice string) (string, error) {
	if inputTokens < 0 || cachedInputTokens < 0 || outputTokens < 0 || cachedInputTokens > inputTokens {
		return "", fmt.Errorf("invalid token counts: %w", ErrInvalidDecimal)
	}
	prices := make([]*big.Rat, 3)
	for index, value := range []string{inputPrice, cachedPrice, outputPrice} {
		canonical, err := ParsePrice(value)
		if err != nil {
			return "", fmt.Errorf("invalid price: %w", err)
		}
		parsed, _ := new(big.Rat).SetString(canonical)
		prices[index] = parsed
	}
	uncached := inputTokens - cachedInputTokens
	total := new(big.Rat).Mul(big.NewRat(uncached, 1), prices[0])
	total.Add(total, new(big.Rat).Mul(big.NewRat(cachedInputTokens, 1), prices[1]))
	total.Add(total, new(big.Rat).Mul(big.NewRat(outputTokens, 1), prices[2]))
	total.Quo(total, big.NewRat(1_000_000, 1))
	return roundAmount(total)
}

func roundAmount(value *big.Rat) (string, error) {
	result := roundFixed(value, AmountScale)
	if _, err := parseBounded(result, AmountScale, true, true); err != nil {
		return "", fmt.Errorf("amount exceeds NUMERIC(30,12): %w", ErrInvalidDecimal)
	}
	return result, nil
}

func roundFixed(value *big.Rat, scale int) string {
	factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	scaledNumerator := new(big.Int).Mul(value.Num(), factor)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(scaledNumerator, value.Denom(), remainder)
	if remainder.Sign() != 0 {
		twiceRemainder := new(big.Int).Lsh(new(big.Int).Abs(remainder), 1)
		if twiceRemainder.Cmp(value.Denom()) >= 0 {
			if value.Sign() < 0 {
				quotient.Sub(quotient, big.NewInt(1))
			} else {
				quotient.Add(quotient, big.NewInt(1))
			}
		}
	}
	return scaledIntegerString(quotient, scale)
}

func scaledIntegerString(value *big.Int, scale int) string {
	negative := value.Sign() < 0
	digits := new(big.Int).Abs(value).String()
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	formatted := digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	if negative && value.Sign() != 0 {
		formatted = "-" + formatted
	}
	return formatted
}
