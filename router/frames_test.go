package router

import "testing"

func TestFrameworkFunctionSkipsFrameworkAndStandardLibraryFrames(t *testing.T) {
	t.Parallel()
	framework := frameworkPackage
	for function, want := range map[string]bool{
		// The framework, the standard library and the runtime never register
		// a route on the application's behalf.
		framework + "/router.(*Router).Handle": true,
		framework + ".(*App).Handle":           true,
		framework + "/httpserver.New":          true,
		"sync.(*Once).Do":                      true,
		"sync.(*Once).doSlow":                  true,
		"runtime.goexit":                       true,
		"runtime.main":                         true,
		"testing.tRunner":                      true,
		"net/http.HandlerFunc.ServeHTTP":       true,
		"net/http.(*ServeMux).Handle":          true,
		// Application code is reported: its own module, package main, the
		// framework's examples and tests of any package.
		"example.com/shop/app.Register":                 false,
		"example.com/shop/app.(*Handlers).routes.func1": false,
		"example.com/shop/cmd/shop.run":                 false,
		"main.main":                                     false,
		framework + "/examples/basic-api/app.Configure": false,
		framework + "/router_test.TestRegistrations":    false,
		framework + "-examples/app.Register":            false,
	} {
		if got := frameworkFunction(function); got != want {
			t.Errorf("frameworkFunction(%q) = %v, want %v", function, got, want)
		}
	}
}
