package webui

import "testing"

func TestCompareSizesUsesTolerance(t *testing.T) {
	tests := []struct {
		name     string
		current  int64
		recorded int64
		want     string
	}{
		{
			name:     "exact match",
			current:  10 << 30,
			recorded: 10 << 30,
			want:     "match",
		},
		{
			name:     "tiny growth is still a match",
			current:  (10 << 30) + sizeCompareToleranceBytes,
			recorded: 10 << 30,
			want:     "match",
		},
		{
			name:     "tiny shrink is still a match",
			current:  (10 << 30) - sizeCompareToleranceBytes,
			recorded: 10 << 30,
			want:     "match",
		},
		{
			name:     "meaningful growth is flagged",
			current:  (10 << 30) + sizeCompareToleranceBytes + 1,
			recorded: 10 << 30,
			want:     "grew",
		},
		{
			name:     "meaningful shrink is flagged",
			current:  (10 << 30) - sizeCompareToleranceBytes - 1,
			recorded: 10 << 30,
			want:     "shrank",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compareSizes(tt.current, tt.recorded); got != tt.want {
				t.Fatalf("compareSizes(%d, %d) = %q, want %q", tt.current, tt.recorded, got, tt.want)
			}
		})
	}
}
