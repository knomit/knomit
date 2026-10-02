//go:build windows

package main

import "strings"

// On Windows the local listener is a named pipe: the address spelling is
// npipe:////./pipe/<name>. The argument test hands these a unix-style path;
// its last element becomes the pipe name.
func localArg(path string) string {
	return "npipe:////./pipe/" + path[strings.LastIndex(path, "/")+1:]
}

func localPath(path string) string {
	return `\\.\pipe\` + path[strings.LastIndex(path, "/")+1:]
}
