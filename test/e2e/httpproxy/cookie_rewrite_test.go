// Copyright Project Contour Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build e2e
// +build e2e

package httpproxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	contourv1 "github.com/projectcontour/contour/apis/projectcontour/v1"
	"github.com/projectcontour/contour/internal/ref"
	"github.com/projectcontour/contour/test/e2e"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func testInvalidCookieRewriteFields(namespace string) {
	Specify("cookie rewrite fields are validated", func() {
		controlChars := []rune{}
		for i := rune(0); i < 32; i++ {
			controlChars = append(controlChars, i)
		}

		invalidNameChars := append([]rune{
			// Separators, whitespace, DEL, and control chars.
			'(', ')', '<', '>', '@', ',', ';', ':', '\\', '"', '/', '[', ']', '?', '=', '{', '}', ' ', '\t', 127,
		}, controlChars...)

		for _, c := range invalidNameChars {
			p := &contourv1.HTTPProxy{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      fmt.Sprintf("invalid-cookie-name-%d", c),
				},
				Spec: contourv1.HTTPProxySpec{
					VirtualHost: &contourv1.VirtualHost{
						Fqdn: fmt.Sprintf("invalid-cookie-name-%d.projectcontour.io", c),
					},
					Routes: []contourv1.Route{
						{
							CookieRewritePolicies: []contourv1.CookieRewritePolicy{
								{
									Name:        fmt.Sprintf("invalid%cchar", c),
									PathRewrite: &contourv1.CookiePathRewrite{Value: "/foo"},
								},
							},
							Services: []contourv1.Service{
								{
									Name: "echo",
									Port: 80,
								},
							},
						},
					},
				},
			}
			assert.Error(f.T(), f.Client.Create(context.TODO(), p), "expected char %d to be invalid in cookie name", c)
		}

		// ;, DEL, and control chars.
		invalidPathChars := append([]rune{';', 127}, controlChars...)
		for _, c := range invalidPathChars {
			p := &contourv1.HTTPProxy{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      fmt.Sprintf("invalid-path-%d", c),
				},
				Spec: contourv1.HTTPProxySpec{
					VirtualHost: &contourv1.VirtualHost{
						Fqdn: fmt.Sprintf("invalid-path-%d.projectcontour.io", c),
					},
					Routes: []contourv1.Route{
						{
							CookieRewritePolicies: []contourv1.CookieRewritePolicy{
								{
									Name:        "invalidpath",
									PathRewrite: &contourv1.CookiePathRewrite{Value: fmt.Sprintf("/invalid%cpath", c)},
								},
							},
							Services: []contourv1.Service{
								{
									Name: "echo",
									Port: 80,
								},
							},
						},
					},
				},
			}
			assert.Error(f.T(), f.Client.Create(context.TODO(), p), "expected char %d to be invalid in path rewrite", c)
		}

		invalidDomains := []string{
			"*", "*.foo.com", "invalid.char&.com",
		}
		for i, d := range invalidDomains {
			p := &contourv1.HTTPProxy{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      fmt.Sprintf("invalid-domain-%d", i),
				},
				Spec: contourv1.HTTPProxySpec{
					VirtualHost: &contourv1.VirtualHost{
						Fqdn: fmt.Sprintf("invalid-domain-%d.projectcontour.io", i),
					},
					Routes: []contourv1.Route{
						{
							CookieRewritePolicies: []contourv1.CookieRewritePolicy{
								{
									Name: "invaliddomain",
									DomainRewrite: &contourv1.CookieDomainRewrite{
										Value: d,
									},
								},
							},
							Services: []contourv1.Service{
								{
									Name: "echo",
									Port: 80,
								},
							},
						},
					},
				},
			}
			assert.Error(f.T(), f.Client.Create(context.TODO(), p), "expected domain rewrite %q to be invalid", d)
		}

		p := &contourv1.HTTPProxy{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      "invalid-samesite",
			},
			Spec: contourv1.HTTPProxySpec{
				VirtualHost: &contourv1.VirtualHost{
					Fqdn: "invalid-samesite.projectcontour.io",
				},
				Routes: []contourv1.Route{
					{
						CookieRewritePolicies: []contourv1.CookieRewritePolicy{
							{
								Name:     "invalid-samesite",
								SameSite: ref.To("Invalid"),
							},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
							},
						},
					},
				},
			},
		}
		assert.Error(f.T(), f.Client.Create(context.TODO(), p), "expected invalid SameSite to be rejected")
	})
}

func testAppCookieRewrite(namespace string) {
	Specify("cookies from app can be rewritten", func() {
		deployEchoServer(f.T(), f.Client, namespace, "echo")
		deployEchoServer(f.T(), f.Client, namespace, "echo-other")

		p := &contourv1.HTTPProxy{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      "app-cookie-rewrite",
			},
			Spec: contourv1.HTTPProxySpec{
				VirtualHost: &contourv1.VirtualHost{
					Fqdn: "app-cookie-rewrite.projectcontour.io",
				},
				Routes: []contourv1.Route{
					{
						Conditions: []contourv1.MatchCondition{
							{Prefix: "/no-rewrite"},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
							},
						},
					},
					{
						Conditions: []contourv1.MatchCondition{
							{Prefix: "/no-attributes"},
						},
						CookieRewritePolicies: []contourv1.CookieRewritePolicy{
							{
								Name:          "no-attributes",
								PathRewrite:   &contourv1.CookiePathRewrite{Value: "/foo"},
								DomainRewrite: &contourv1.CookieDomainRewrite{Value: "foo.com"},
								Secure:        ref.To(true),
								SameSite:      ref.To("Strict"),
							},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
							},
						},
					},
					{
						Conditions: []contourv1.MatchCondition{
							{Prefix: "/rewrite-all"},
						},
						CookieRewritePolicies: []contourv1.CookieRewritePolicy{
							{
								Name:          "rewrite-all",
								PathRewrite:   &contourv1.CookiePathRewrite{Value: "/ra"},
								DomainRewrite: &contourv1.CookieDomainRewrite{Value: "ra.com"},
								Secure:        ref.To(false),
								SameSite:      ref.To("Lax"),
							},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
							},
						},
					},
					{
						Conditions: []contourv1.MatchCondition{
							{Prefix: "/rewrite-some"},
						},
						CookieRewritePolicies: []contourv1.CookieRewritePolicy{
							{
								Name:          "rewrite-some",
								DomainRewrite: &contourv1.CookieDomainRewrite{Value: "rs.com"},
							},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
							},
						},
					},
					{
						Conditions: []contourv1.MatchCondition{
							{Prefix: "/multi"},
						},
						CookieRewritePolicies: []contourv1.CookieRewritePolicy{
							{
								Name:        "multi-1",
								PathRewrite: &contourv1.CookiePathRewrite{Value: "/m1"},
							},
							{
								Name:          "multi-2",
								DomainRewrite: &contourv1.CookieDomainRewrite{Value: "m2.com"},
							},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
							},
						},
					},
					{
						Conditions: []contourv1.MatchCondition{
							{Prefix: "/service"},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
								CookieRewritePolicies: []contourv1.CookieRewritePolicy{
									{
										Name:        "service",
										PathRewrite: &contourv1.CookiePathRewrite{Value: "/svc-new"},
									},
								},
							},
							{
								Name: "echo-other",
								Port: 80,
								CookieRewritePolicies: []contourv1.CookieRewritePolicy{
									{
										Name:        "service",
										PathRewrite: &contourv1.CookiePathRewrite{Value: "/svc-new-other"},
									},
								},
							},
						},
					},
					{
						Conditions: []contourv1.MatchCondition{
							{Prefix: "/route-and-service"},
						},
						CookieRewritePolicies: []contourv1.CookieRewritePolicy{
							{
								Name:          "route-service",
								PathRewrite:   &contourv1.CookiePathRewrite{Value: "/route"},
								DomainRewrite: &contourv1.CookieDomainRewrite{Value: "route.com"},
							},
							{
								Name:        "route",
								PathRewrite: &contourv1.CookiePathRewrite{Value: "/route"},
							},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
								CookieRewritePolicies: []contourv1.CookieRewritePolicy{
									{
										Name:        "route-service",
										PathRewrite: &contourv1.CookiePathRewrite{Value: "/service"},
										Secure:      ref.To(true),
										SameSite:    ref.To("Lax"),
									},
									{
										Name:        "service",
										PathRewrite: &contourv1.CookiePathRewrite{Value: "/service"},
									},
								},
							},
						},
					},
				},
			},
		}
		f.CreateHTTPProxyAndWaitFor(p, e2e.HTTPProxyValid)

		// No rewrite rule on route, nothing should change.
		headers := requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/no-rewrite", "no-rewrite=foo; Path=/nrw; Domain=nrw.com; SameSite=Strict; Secure")
		checkReturnedSetCookieHeader(headers, "no-rewrite", "foo", "/nrw", "nrw.com", "Strict", true, nil)

		// Cookie that should not be rewritten by this route (but is rewritten by another).
		// Note: testing that cookie rewrites are per-route and don't leak across (i.e. not a global Lua filter).
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/no-rewrite", "rewrite-all=foo; Path=/ra; Domain=ra.com; SameSite=Strict; Secure")
		checkReturnedSetCookieHeader(headers, "rewrite-all", "foo", "/ra", "ra.com", "Strict", true, nil)

		// No original cookie attributes, all added.
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/no-attributes", "no-attributes=foo")
		checkReturnedSetCookieHeader(headers, "no-attributes", "foo", "/foo", "foo.com", "Strict", true, nil)

		// Rewrite all available attributes.
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/rewrite-all", "rewrite-all=bar; Path=/bar; Domain=bar.com; SameSite=None; Secure")
		checkReturnedSetCookieHeader(headers, "rewrite-all", "bar", "/ra", "ra.com", "Lax", false, nil)

		// Non-rewritable attributes are untouched.
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/rewrite-all", "rewrite-all=baz; Domain=bar.com; Max-Age=10; HttpOnly")
		checkReturnedSetCookieHeader(headers, "rewrite-all", "baz", "/ra", "ra.com", "Lax", false, map[string]string{"Max-Age": "10", "HttpOnly": ""})

		// Rewrite some available attributes (i.e. original attributes are retained with no rewrite policy).
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/rewrite-some", "rewrite-some=bar; Path=/bar; Domain=bar.com; SameSite=Lax; Secure")
		checkReturnedSetCookieHeader(headers, "rewrite-some", "bar", "/bar", "rs.com", "Lax", true, nil)

		// Multiple rewrites on a route, check with individual responses.
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/multi", "multi-1=bar")
		checkReturnedSetCookieHeader(headers, "multi-1", "bar", "/m1", "", "", false, nil)
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/multi", "multi-2=bar")
		checkReturnedSetCookieHeader(headers, "multi-2", "bar", "", "m2.com", "", false, nil)

		// TODO: currently the test server will not return multiple headers with the same name,
		// need to test multiple Set-Cookie headers in the same response.
		// Multiple rewrites on a route, first check with multiple Set-Cookie headers in response.
		// headers = requestSetCookieHeader("/multi", "multi-1=bar", "multi-2=bar")
		// checkReturnedSetCookieHeader(headers, "multi-1", "bar", "/m1", "", "", false, nil)
		// checkReturnedSetCookieHeader(headers, "multi-2", "bar", "", "m2.com", "", false, nil)

		// Rewrite on a service, balancing to multiple services.
		services := map[string]struct{}{}
		// Use a few attempts to make sure we hit both services.
		for i := 0; i < 20; i++ {
			headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/service", "service=baz; Path=/svc")
			for headerName, values := range headers {
				if headerName != "Set-Cookie" {
					continue
				}
				for _, v := range values {
					if strings.Contains(v, "Path=/svc-new-other") {
						services["echo-other"] = struct{}{}
					} else if strings.Contains(v, "Path=/svc-new") {
						services["echo"] = struct{}{}
					}
				}
			}
		}
		// Make sure both services/rewrites have been reached.
		assert.Contains(f.T(), services, "echo")
		assert.Contains(f.T(), services, "echo-other")

		// Rewrite on a route and service.
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/route-and-service", "route-service=baz")
		checkReturnedSetCookieHeader(headers, "route-service", "baz", "/service", "route.com", "Lax", true, nil)
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/route-and-service", "route=baz")
		checkReturnedSetCookieHeader(headers, "route", "baz", "/route", "", "", false, nil)
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/route-and-service", "service=baz")
		checkReturnedSetCookieHeader(headers, "service", "baz", "/service", "", "", false, nil)

		// Error case, invalid cookie (invalid name=value pair) should be untouched.
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/rewrite-some", "rewrite-some")
		checkReturnedSetCookieHeader(headers, "rewrite-some", "", "", "", "", false, nil)

		// Ends with ;
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/rewrite-some", "rewrite-some=foo; Path=/;")
		checkReturnedSetCookieHeader(headers, "rewrite-some", "foo", "/", "rs.com", "", false, nil)

		// Ends with spaces
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/rewrite-some", "rewrite-some=foo;   ")
		checkReturnedSetCookieHeader(headers, "rewrite-some", "foo", "", "rs.com", "", false, nil)

		// Empty attribute
		headers = requestSetCookieHeader(false, p.Spec.VirtualHost.Fqdn, "/rewrite-some", "rewrite-some=foo; ;")
		checkReturnedSetCookieHeader(headers, "rewrite-some", "foo", "", "rs.com", "", false, nil)
	})
}

func testHeaderGlobalRewriteCookieRewrite(namespace string) {
	Specify("cookies from global header rewrites can be rewritten", func() {
		f.Fixtures.Echo.Deploy(namespace, "echo")

		p := &contourv1.HTTPProxy{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      "global-header-rewrite-cookie-rewrite",
			},
			Spec: contourv1.HTTPProxySpec{
				VirtualHost: &contourv1.VirtualHost{
					Fqdn: "global-header-rewrite-cookie-rewrite.projectcontour.io",
				},
				Routes: []contourv1.Route{
					{
						Conditions: []contourv1.MatchCondition{
							{Prefix: "/global"},
						},
						CookieRewritePolicies: []contourv1.CookieRewritePolicy{
							{
								Name:        "global",
								PathRewrite: &contourv1.CookiePathRewrite{Value: "/global"},
							},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
							},
						},
					},
				},
			},
		}
		f.CreateHTTPProxyAndWaitFor(p, e2e.HTTPProxyValid)

		res, ok := f.HTTP.RequestUntil(&e2e.HTTPRequestOpts{
			Path:      "/global",
			Host:      p.Spec.VirtualHost.Fqdn,
			Condition: e2e.HasStatusCode(200),
		})
		require.NotNil(f.T(), res)
		require.Truef(f.T(), ok, "expected 200 response code, got %d", res.StatusCode)
		checkReturnedSetCookieHeader(res.Headers, "global", "foo", "/global", "", "", false, nil)
	})
}

func testHeaderRewriteCookieRewrite(namespace string) {
	Specify("cookies from HTTPProxy header rewrites can be rewritten", func() {
		f.Fixtures.Echo.Deploy(namespace, "echo")

		p := &contourv1.HTTPProxy{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      "header-rewrite-cookie-rewrite",
			},
			Spec: contourv1.HTTPProxySpec{
				VirtualHost: &contourv1.VirtualHost{
					Fqdn: "header-rewrite-cookie-rewrite.projectcontour.io",
				},
				Routes: []contourv1.Route{
					{
						Conditions: []contourv1.MatchCondition{
							{Prefix: "/cookie-lb"},
						},
						LoadBalancerPolicy: &contourv1.LoadBalancerPolicy{
							Strategy: "Cookie",
						},
						CookieRewritePolicies: []contourv1.CookieRewritePolicy{
							{
								Name:     "X-Contour-Session-Affinity",
								Secure:   ref.To(true),
								SameSite: ref.To("Strict"),
							},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
							},
						},
					},
					{
						Conditions: []contourv1.MatchCondition{
							{Prefix: "/route-route"},
						},
						ResponseHeadersPolicy: &contourv1.HeadersPolicy{
							Set: []contourv1.HeaderValue{
								{Name: "Set-Cookie", Value: "route-route=foo"},
							},
						},
						CookieRewritePolicies: []contourv1.CookieRewritePolicy{
							{
								Name:        "route-route",
								PathRewrite: &contourv1.CookiePathRewrite{Value: "/route-route"},
							},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
							},
						},
					},
					{
						Conditions: []contourv1.MatchCondition{
							{Prefix: "/route-service"},
						},
						ResponseHeadersPolicy: &contourv1.HeadersPolicy{
							Set: []contourv1.HeaderValue{
								{Name: "Set-Cookie", Value: "route-service=foo"},
							},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
								CookieRewritePolicies: []contourv1.CookieRewritePolicy{
									{
										Name:        "route-service",
										PathRewrite: &contourv1.CookiePathRewrite{Value: "/route-service"},
									},
								},
							},
						},
					},
					{
						Conditions: []contourv1.MatchCondition{
							{Prefix: "/service-service"},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
								ResponseHeadersPolicy: &contourv1.HeadersPolicy{
									Set: []contourv1.HeaderValue{
										{Name: "Set-Cookie", Value: "service-service=bar"},
									},
								},
								CookieRewritePolicies: []contourv1.CookieRewritePolicy{
									{
										Name:        "service-service",
										PathRewrite: &contourv1.CookiePathRewrite{Value: "/service-service"},
									},
								},
							},
						},
					},
					{
						Conditions: []contourv1.MatchCondition{
							{Prefix: "/service-route"},
						},
						CookieRewritePolicies: []contourv1.CookieRewritePolicy{
							{
								Name:        "service-route",
								PathRewrite: &contourv1.CookiePathRewrite{Value: "/service-route"},
							},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
								ResponseHeadersPolicy: &contourv1.HeadersPolicy{
									Set: []contourv1.HeaderValue{
										{Name: "Set-Cookie", Value: "service-route=bar"},
									},
								},
							},
						},
					},
				},
			},
		}
		f.CreateHTTPProxyAndWaitFor(p, e2e.HTTPProxyValid)

		res, ok := f.HTTP.RequestUntil(&e2e.HTTPRequestOpts{
			Path:      "/cookie-lb",
			Host:      p.Spec.VirtualHost.Fqdn,
			Condition: e2e.HasStatusCode(200),
		})
		require.NotNil(f.T(), res)
		require.Truef(f.T(), ok, "expected 200 response code, got %d", res.StatusCode)
		checkReturnedSetCookieHeader(res.Headers, "X-Contour-Session-Affinity", "", "/", "", "Strict", true, map[string]string{"HttpOnly": ""})

		res, ok = f.HTTP.RequestUntil(&e2e.HTTPRequestOpts{
			Path:      "/route-route",
			Host:      p.Spec.VirtualHost.Fqdn,
			Condition: e2e.HasStatusCode(200),
		})
		require.NotNil(f.T(), res)
		require.Truef(f.T(), ok, "expected 200 response code, got %d", res.StatusCode)
		checkReturnedSetCookieHeader(res.Headers, "route-route", "foo", "/route-route", "", "", false, nil)

		res, ok = f.HTTP.RequestUntil(&e2e.HTTPRequestOpts{
			Path:      "/route-service",
			Host:      p.Spec.VirtualHost.Fqdn,
			Condition: e2e.HasStatusCode(200),
		})
		require.NotNil(f.T(), res)
		require.Truef(f.T(), ok, "expected 200 response code, got %d", res.StatusCode)
		checkReturnedSetCookieHeader(res.Headers, "route-service", "foo", "/route-service", "", "", false, nil)

		res, ok = f.HTTP.RequestUntil(&e2e.HTTPRequestOpts{
			Path:      "/service-service",
			Host:      p.Spec.VirtualHost.Fqdn,
			Condition: e2e.HasStatusCode(200),
		})
		require.NotNil(f.T(), res)
		require.Truef(f.T(), ok, "expected 200 response code, got %d", res.StatusCode)
		checkReturnedSetCookieHeader(res.Headers, "service-service", "bar", "/service-service", "", "", false, nil)

		res, ok = f.HTTP.RequestUntil(&e2e.HTTPRequestOpts{
			Path:      "/service-route",
			Host:      p.Spec.VirtualHost.Fqdn,
			Condition: e2e.HasStatusCode(200),
		})
		require.NotNil(f.T(), res)
		require.Truef(f.T(), ok, "expected 200 response code, got %d", res.StatusCode)
		checkReturnedSetCookieHeader(res.Headers, "service-route", "bar", "/service-route", "", "", false, nil)
	})
}

func testCookieRewriteTLS(namespace string) {
	Specify("cookies rewrites work on TLS vhosts", func() {
		deployEchoServer(f.T(), f.Client, namespace, "echo")
		f.Certs.CreateSelfSignedCert(namespace, "echo-cert", "echo", "cookie-rewrite-tls.projectcontour.io")

		p := &contourv1.HTTPProxy{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      "cookie-rewrite-tls",
			},
			Spec: contourv1.HTTPProxySpec{
				VirtualHost: &contourv1.VirtualHost{
					Fqdn: "cookie-rewrite-tls.projectcontour.io",
					TLS: &contourv1.TLS{
						SecretName: "echo",
					},
				},
				Routes: []contourv1.Route{
					{
						CookieRewritePolicies: []contourv1.CookieRewritePolicy{
							{
								Name:          "a-cookie",
								PathRewrite:   &contourv1.CookiePathRewrite{Value: "/"},
								DomainRewrite: &contourv1.CookieDomainRewrite{Value: "cookie-rewrite-tls.projectcontour.io"},
								Secure:        ref.To(true),
								SameSite:      ref.To("Strict"),
							},
						},
						Services: []contourv1.Service{
							{
								Name: "echo",
								Port: 80,
							},
						},
					},
				},
			},
		}
		f.CreateHTTPProxyAndWaitFor(p, e2e.HTTPProxyValid)

		headers := requestSetCookieHeader(true, p.Spec.VirtualHost.Fqdn, "/", "a-cookie=bar; Domain=cookie-rewrite-tls.projectcontour.io; Path=/; SameSite=Strict; Secure")
		checkReturnedSetCookieHeader(headers, "a-cookie", "bar", "/", "cookie-rewrite-tls.projectcontour.io", "Strict", true, nil)

		// Make sure SNI mismatch check still works (implemented in Lua)
		res, ok := f.HTTP.SecureRequestUntil(&e2e.HTTPSRequestOpts{
			Host: "non-matching-host.projectcontour.io",
			TLSConfigOpts: []func(*tls.Config){
				e2e.OptSetSNI(p.Spec.VirtualHost.Fqdn),
			},
			Condition: e2e.HasStatusCode(421),
		})
		require.Truef(f.T(), ok, "expected 421 (Misdirected Request) response code, got %d", res.StatusCode)
	})
}

// Find cookie and parse attributes from set of headers.
func parseCookieAttributes(headers http.Header, cookieName string) map[string]string {
	attributes := map[string]string{}
	for headerName, values := range headers {
		if headerName != "Set-Cookie" {
			continue
		}
		for _, v := range values {
			if strings.HasPrefix(v, cookieName) {
				attributePairs := strings.Split(v, ";")
				for _, p := range attributePairs {
					split := strings.Split(strings.TrimSpace(p), "=")
					if len(split) == 0 {
						continue
					}
					attributeName := split[0]
					attributeValue := ""
					if len(split) > 1 {
						attributeValue = split[1]
					}
					attributes[attributeName] = attributeValue
				}
			}
		}
	}
	return attributes
}

func checkReturnedSetCookieHeader(headers http.Header, cookieName, cookieValue, path, domain, sameSite string, secure bool, additionalAttrs map[string]string) {
	f.T().Helper()

	attributes := parseCookieAttributes(headers, cookieName)

	// If cookie value is empty we ignore it (e.g. a generated cookie we don't know the value of)
	if len(cookieValue) > 0 {
		assert.Equal(f.T(), cookieValue, attributes[cookieName])
	}
	if len(path) > 0 {
		assert.Equal(f.T(), path, attributes["Path"])
	} else {
		assert.NotContains(f.T(), attributes, "Path")
	}
	if len(domain) > 0 {
		assert.Equal(f.T(), domain, attributes["Domain"])
	} else {
		assert.NotContains(f.T(), attributes, "Domain")
	}
	if len(sameSite) > 0 {
		assert.Equal(f.T(), sameSite, attributes["SameSite"])
	} else {
		assert.NotContains(f.T(), attributes, "SameSite")
	}
	if secure {
		assert.Contains(f.T(), attributes, "Secure")
	} else {
		assert.NotContains(f.T(), attributes, "Secure")
	}

	for a, v := range additionalAttrs {
		assert.Equal(f.T(), v, attributes[a])
	}
}

func requestSetCookieHeader(https bool, host, route string, setCookieValues ...string) http.Header {
	f.T().Helper()
	opts := []func(*http.Request){
		e2e.OptSetHeaders(map[string]string{
			"X-ECHO-HEADER": "Set-Cookie:" + strings.Join(setCookieValues, ", Set-Cookie:"),
		}),
	}

	var res *e2e.HTTPResponse
	var ok bool
	if https {
		res, ok = f.HTTP.SecureRequestUntil(&e2e.HTTPSRequestOpts{
			Path:        route,
			Host:        host,
			RequestOpts: opts,
			Condition:   e2e.HasStatusCode(200),
		})
	} else {
		res, ok = f.HTTP.RequestUntil(&e2e.HTTPRequestOpts{
			Path:        route,
			Host:        host,
			RequestOpts: opts,
			Condition:   e2e.HasStatusCode(200),
		})
	}

	require.NotNil(f.T(), res)
	require.Truef(f.T(), ok, "expected 200 response code, got %d", res.StatusCode)

	return res.Headers
}

func deployEchoServer(t require.TestingT, c client.Client, ns, name string) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app.kubernetes.io/name": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app.kubernetes.io/name": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "echo",
							Image: "docker.io/ealen/echo-server:0.5.1",
							Env: []corev1.EnvVar{
								{
									Name:  "INGRESS_NAME",
									Value: name,
								},
								{
									Name:  "SERVICE_NAME",
									Value: name,
								},
								{
									Name: "POD_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.name",
										},
									},
								},
								{
									Name: "NAMESPACE",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.namespace",
										},
									},
								},
								{
									Name:  "PORT",
									Value: "3000",
								},
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: 3000,
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/ping",
										Port: intstr.FromInt(3000),
									},
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, c.Create(context.TODO(), deployment))

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       80,
					TargetPort: intstr.FromString("http"),
				},
			},
			Selector: map[string]string{"app.kubernetes.io/name": name},
		},
	}
	require.NoError(t, c.Create(context.TODO(), service))
}
