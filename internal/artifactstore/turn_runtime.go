package artifactstore

const TurnRequestField = "turn_request"

func TurnContains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
