package router

import "testing"

func TestSkippedFrameSkipsFrameworkAndStandardLibraryFrames(t *testing.T) {
	t.Parallel()
	framework := frameworkPackage
	for _, tc := range []struct {
		function, mainModule string
		skipped              bool
	}{
		// The framework, the standard library and the runtime never register
		// a route on the application's behalf.
		{framework + "/router.(*Router).Handle", "example.com/shop", true},
		{framework + ".(*App).Handle", "example.com/shop", true},
		{framework + "/httpserver.New", "myapp", true},
		{"sync.(*Once).Do", "example.com/shop", true},
		{"sync.(*Once).doSlow", "myapp", true},
		{"runtime.goexit", "myapp", true},
		{"runtime.main", "", true},
		{"testing.tRunner", "example.com/shop", true},
		{"net/http.HandlerFunc.ServeHTTP", "myapp", true},
		{"net/http.(*ServeMux).Handle", "", true},
		// Application code is reported: its own module, package main, the
		// framework's examples and tests of any package.
		{"example.com/shop/app.Register", "example.com/shop", false},
		{"example.com/shop/app.(*Handlers).routes.func1", "", false},
		{"example.com/shop/cmd/shop.run", "", false},
		{"main.main", "", false},
		{framework + "/examples/basic-api/app.Configure", "", false},
		{framework + "/router_test.TestRegistrations", "", false},
		{framework + "-examples/app.Register", "", false},
		// A module made by `forge new myapp` is "myapp", a path with no dot
		// that looks like the standard library: the main module is always
		// application code, in its subpackages and at its root.
		{"myapp/internal/app.Configure", "myapp", false},
		{"myapp/internal/app.(*Handlers).routes.func1", "myapp", false},
		{"myapp/cmd/myapp.run", "myapp", false},
		{"myapp.Register", "myapp", false},
		// Only the main module: another dotless path is not it, and without
		// build information the dotless rule applies.
		{"myapp2/internal/app.Configure", "myapp", true},
		{"other/internal/app.Configure", "myapp", true},
		{"myapp/internal/app.Configure", "", true},
	} {
		if got := skippedFrame(tc.function, tc.mainModule); got != tc.skipped {
			t.Errorf("skippedFrame(%q, main module %q) = %v, want %v", tc.function, tc.mainModule, got, tc.skipped)
		}
	}
}
