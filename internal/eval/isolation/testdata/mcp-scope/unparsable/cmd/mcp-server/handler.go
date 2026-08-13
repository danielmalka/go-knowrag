// Deliberately not valid Go. A scan that shrugged at a file it could not read would report the
// binding as held over source nobody parsed. Not built or linted: testdata is ignored by the go
// tool.
package main

func broken( {
