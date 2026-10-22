package crdt

import "fmt"

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
func sprint(args ...any) string                 { return fmt.Sprint(args...) }
