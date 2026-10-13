package treebackup

// walkSeams are test hooks into the walker, to stage races at exact points.
// Every field is nil outside tests.
var walkSeams struct {
	// stat may rewrite an fstatat result in pass 1.
	stat func(rel []byte, st *sysStat)
	// beforeOpen runs between the stat and the open of a directory in
	// pass 1 and of a file in pass 2.
	beforeOpen func(rel []byte)
	// beforeList runs before a directory's listing fd is opened in phase B.
	beforeList func(rel []byte)
	// listed sees each chunk of names read from a directory and may change it.
	listed func(rel []byte, chunk int, names []string) []string
	// afterChunk runs after each chunk of file content is copied in pass 2.
	afterChunk func(rel []byte, attempt int, off int64)
}
