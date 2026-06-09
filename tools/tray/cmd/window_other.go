//go:build !darwin

package cmd

import webview "github.com/webview/webview_go"

func customizeWindow(wv webview.WebView) {}
