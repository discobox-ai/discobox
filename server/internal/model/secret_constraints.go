package model

// RestrictRoutes adds an independent policy list, preserving all prior
// restrictions, including empty lists (deny everywhere). Normalization makes
// retries idempotent. Flattening these lists would lose blocked carve-outs.
func (s *Secret) RestrictRoutes(routes []SecretRoute) (bool, error) {
	next, err := NormalizeRoutes(routes)
	if err != nil {
		return false, err
	}
	if SameRoutes(s.Routes, next) {
		return false, nil
	}
	for _, prior := range s.RouteConstraints {
		if SameRoutes(prior, next) {
			return false, nil
		}
	}
	// Keep an empty list as [] rather than null, including on API reads.
	if next == nil {
		next = []SecretRoute{}
	}
	s.RouteConstraints = append(s.RouteConstraints, next)
	return true, nil
}
