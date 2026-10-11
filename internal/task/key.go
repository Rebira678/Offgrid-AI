package task

// TitleKey is the form of a title used to decide whether two titles are "the
// same task": lower case, runs of whitespace collapsed. A persistent store
// keeps it in a column so duplicate detection is an indexed lookup.
func TitleKey(title string) string { return fold(title) }
