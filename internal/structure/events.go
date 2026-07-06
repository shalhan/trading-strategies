package structure

import (
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/strategy"
)

// Event is one observable moment in the engine's decision process, emitted when
// an event hook is attached via SetEventHook. Events are purely observational —
// a hook never alters engine behavior — and exist so a UI (cmd/viz) can replay
// exactly what the engine saw and decided, for visual verification.
//
// Types:
//
//	pivot_high / pivot_low — a swing pivot confirmed (Time = the pivot candle,
//	    which is PivotN bars before the confirming bar); Price = the swing level.
//	sweep          — liquidity-sweep mode: a wick beyond a swing that closed back
//	    inside; Level = swept swing, Price = the wick extreme.
//	break          — a close broke a swing level; Setup = BOS or CHoCH,
//	    Level = the broken swing, Price = the breaking close.
//	skip           — a break (or fill) did NOT become an entry; Reason says why.
//	fvg_limit      — a limit order rested in a Fair Value Gap; Lo/Hi = the gap,
//	    Level = the limit price, Time2 = open of the impulse candle that
//	    started the gap; Stop/Target as computed.
//	limit_filled   — the resting limit was touched (entry proposed to the
//	    portfolio); Price = fill level.
//	limit_cancelled — the resting limit expired or was invalidated; see Reason.
//	entry          — a position opened (portfolio accepted); Price/Stop/Target set.
//	stop_move      — the stop moved (Reason: bos_trail, break_even, scale_out);
//	    Level = old stop, Price = new stop.
//	exit           — the position closed; Price = exit, R = result in R,
//	    Reason = outcome (stop/target/trail).
type Event struct {
	Type   string    `json:"type"`
	Time   time.Time `json:"time"`
	Time2  time.Time `json:"time2,omitzero"`
	Side   string    `json:"side,omitempty"`
	Setup  string    `json:"setup,omitempty"`
	Price  float64   `json:"price,omitempty"`
	Level  float64   `json:"level,omitempty"`
	Lo     float64   `json:"lo,omitempty"`
	Hi     float64   `json:"hi,omitempty"`
	Stop   float64   `json:"stop,omitempty"`
	Target float64   `json:"target,omitempty"`
	R      float64   `json:"r,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

// SetEventHook attaches an observer called synchronously on every Event. Pass
// nil to detach. The hook must not retain the Engine or call back into it.
func (e *Engine) SetEventHook(fn func(Event)) { e.onEvent = fn }

func (e *Engine) emit(ev Event) {
	if e.onEvent != nil {
		e.onEvent(ev)
	}
}

func sideStr(s strategy.Side) string {
	if s == strategy.Long {
		return "long"
	}
	return "short"
}
