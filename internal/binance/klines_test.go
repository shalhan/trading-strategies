package binance

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/shalhan/orderflow-trading-app/internal/kline"
)

func TestParseKlines(t *testing.T) {
	// Two rows in Binance REST array form (numbers for times, strings for prices).
	sample := `[
      [1700000000000,"1.2000","1.2500","1.1900","1.2400","1000.5",1700000299999,"1234.5",42,"500","600","0"],
      [1700000300000,"1.2400","1.2600","1.2300","1.2550","800.0",1700000599999,"999.9",30,"400","450","0"]
    ]`
	ks, err := ParseKlines(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(ks) != 2 {
		t.Fatalf("got %d klines, want 2", len(ks))
	}
	k0 := ks[0]
	if k0.Open != 1.20 || k0.High != 1.25 || k0.Low != 1.19 || k0.Close != 1.24 || k0.Volume != 1000.5 {
		t.Errorf("row 0 OHLCV wrong: %+v", k0)
	}
	if k0.OpenTime.UnixMilli() != 1700000000000 || k0.CloseTime.UnixMilli() != 1700000299999 {
		t.Errorf("row 0 times wrong: open=%d close=%d", k0.OpenTime.UnixMilli(), k0.CloseTime.UnixMilli())
	}
}

func TestParseKlinesRejectsShortRow(t *testing.T) {
	if _, err := ParseKlines(strings.NewReader(`[[1,"2","3"]]`)); err == nil {
		t.Fatal("expected error on short row")
	}
}

func TestSaveLoadKlinesRoundTrip(t *testing.T) {
	in := []kline.Kline{
		{Open: 1, High: 2, Low: 0.5, Close: 1.5, Volume: 100},
		{Open: 1.5, High: 2.5, Low: 1.4, Close: 2.0, Volume: 200},
	}
	path := filepath.Join(t.TempDir(), "klines", "X-5m.ndjson")
	if err := SaveKlines(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadKlines(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d klines, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Open != in[i].Open || out[i].Close != in[i].Close || out[i].Volume != in[i].Volume {
			t.Errorf("kline %d mismatch: got %+v want %+v", i, out[i], in[i])
		}
	}
}
