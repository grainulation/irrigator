package cost

func ValidMicrosFloat(f float64) bool {
	_, ok := MicrosFromFloat(f)
	return ok
}
