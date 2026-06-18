package agent

var registered []Detector

// Register adds one detector to the global detector registry.
func Register(detector Detector) {
	if detector.Kind() == KindUnknown {
		registered = append([]Detector{detector}, registered...)
		return
	}
	registered = append(registered, detector)
}

// DefaultDetectors returns registered detectors in evaluation order.
func DefaultDetectors() []Detector {
	return append([]Detector(nil), registered...)
}
