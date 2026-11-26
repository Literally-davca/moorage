package engine

// diskFree is unknown on Windows (Moorage manages Linux Docker hosts).
func diskFree(string) (int64, float64) { return -1, -1 }
