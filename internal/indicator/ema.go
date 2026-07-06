package indicator

// EMA is a streaming exponential moving average, fed one value (typically a 5m
// close) at a time. Like ATR it is continuous across days. The strategy uses a
// fast/slow EMA pair to gauge trend for the trend filter.
type EMA struct {
	period int
	mult   float64

	seeded bool
	count  int
	value  float64
}

// NewEMA creates an EMA with the given period (panics on period<1).
func NewEMA(period int) *EMA {
	if period < 1 {
		panic("indicator: EMA period must be >= 1")
	}
	return &EMA{period: period, mult: 2.0 / (float64(period) + 1.0)}
}

// Update feeds the next value and returns the current EMA and whether it is
// ready. It seeds with a simple average of the first `period` values, then
// applies the standard EMA recurrence.
func (e *EMA) Update(v float64) (value float64, ready bool) {
	if !e.seeded {
		e.value += v
		e.count++
		if e.count == e.period {
			e.value /= float64(e.period)
			e.seeded = true
		}
		return e.value, e.seeded
	}
	e.value = v*e.mult + e.value*(1-e.mult)
	return e.value, true
}

// Value returns the current EMA and whether it is ready.
func (e *EMA) Value() (float64, bool) { return e.value, e.seeded }
