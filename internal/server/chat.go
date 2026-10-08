package server

import (
	"encoding/json"
	"math/big"
	"net/http"
	"strconv"
)

// jsToFixed2 renders v like JavaScript's Number.prototype.toFixed(2): the exact
// binary value, ties rounded up (Go's %.2f rounds ties to even, so 0.125 would
// print 0.12 where JS prints 0.13).
func jsToFixed2(v float64) string {
	rat := new(big.Rat)
	if rat.SetFloat64(v) == nil {
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	neg := rat.Sign() < 0
	if neg {
		rat.Neg(rat)
	}
	rat.Mul(rat, big.NewRat(100, 1))
	// floor(x + 1/2)
	rat.Add(rat, big.NewRat(1, 2))
	n := new(big.Int).Quo(rat.Num(), rat.Denom())
	digits := n.String()
	for len(digits) < 3 {
		digits = "0" + digits
	}
	out := digits[:len(digits)-2] + "." + digits[len(digits)-2:]
	if neg && n.Sign() != 0 {
		out = "-" + out
	}
	return out
}

// denyReply is the auth failure the /v1 gate answers with.
type denyReply struct {
	status int
	body   map[string]any
}

func errorBody(typ, message string) map[string]any {
	return map[string]any{"error": map[string]string{"message": message, "type": typ}}
}

func creditLimitBody(limitUSD float64, keyName string) map[string]any {
	return map[string]any{"error": map[string]string{
		"message": "Credit limit reached ($" + jsToFixed2(limitUSD) + ") for API key \"" + keyName +
			"\". Increase or remove the limit in the Jevonian dashboard.",
		"type": "credit_limit_exceeded",
		"code": "credit_limit_exceeded",
	}}
}

func writeJSONError(w http.ResponseWriter, status int, typ, message string) {
	if status <= 0 {
		status = http.StatusBadRequest
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody(typ, message))
}

func itoa(v int) string {
	return strconv.Itoa(v)
}
