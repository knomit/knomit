//go:build darwin

package cmd

/*
#include <objc/runtime.h>
#include <objc/message.h>

typedef unsigned long NSUInteger;
typedef long         NSInteger;
typedef double       CGFloat;

void knomitStyleWindow(void* nswindow) {
	id w = (id)nswindow;

	// Transparent title bar so the window background colour shows through,
	// making it blend with the web app's TopBar (#111111).
	((void (*)(id, SEL, int))objc_msgSend)(
		w, sel_registerName("setTitlebarAppearsTransparent:"), 1);

	// Hide the title text; traffic lights and drag behaviour are unchanged.
	((void (*)(id, SEL, NSInteger))objc_msgSend)(
		w, sel_registerName("setTitleVisibility:"), 1); // NSWindowTitleHidden

	// Match the web app TopBar background (#111 = 17/255 ≈ 0.067).
	CGFloat c = 17.0 / 255.0;
	id color = ((id (*)(id, SEL, CGFloat, CGFloat, CGFloat, CGFloat))objc_msgSend)(
		(id)objc_getClass("NSColor"),
		sel_registerName("colorWithRed:green:blue:alpha:"),
		c, c, c, (CGFloat)1.0);
	((void (*)(id, SEL, id))objc_msgSend)(
		w, sel_registerName("setBackgroundColor:"), color);
}
*/
import "C"

import webview "github.com/webview/webview_go"

func customizeWindow(wv webview.WebView) {
	wv.Dispatch(func() {
		C.knomitStyleWindow(wv.Window())
	})
}
