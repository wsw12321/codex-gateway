package proxy

func validSidecarModelID(model string) bool {
	if model == "" || len(model) > 128 {
		return false
	}
	for _, value := range model {
		if (value < 'a' || value > 'z') && (value < 'A' || value > 'Z') &&
			(value < '0' || value > '9') && value != '-' && value != '_' &&
			value != '.' && value != ':' && value != '/' {
			return false
		}
	}
	return true
}
