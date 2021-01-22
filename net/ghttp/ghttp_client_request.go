// Copyright GoFrame Author(https://goframe.org). All Rights Reserved.
//
// This Source Code Form is subject to the terms of the MIT License.
// If a copy of the MIT was not distributed with this file,
// You can obtain one at https://github.com/gogf/gf.

package ghttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/gogf/gf"
	"github.com/gogf/gf/internal/intlog"
	"github.com/gogf/gf/internal/json"
	"github.com/gogf/gf/internal/utils"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/label"
	"go.opentelemetry.io/otel/trace"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gogf/gf/encoding/gparser"
	"github.com/gogf/gf/text/gregex"
	"github.com/gogf/gf/text/gstr"
	"github.com/gogf/gf/util/gconv"

	"github.com/gogf/gf/os/gfile"
)

// Get send GET request and returns the response object.
// Note that the response object MUST be closed if it'll be never used.
func (c *Client) Get(url string, data ...interface{}) (*ClientResponse, error) {
	return c.DoRequest("GET", url, data...)
}

// Put send PUT request and returns the response object.
// Note that the response object MUST be closed if it'll be never used.
func (c *Client) Put(url string, data ...interface{}) (*ClientResponse, error) {
	return c.DoRequest("PUT", url, data...)
}

// Post sends request using HTTP method POST and returns the response object.
// Note that the response object MUST be closed if it'll be never used.
func (c *Client) Post(url string, data ...interface{}) (*ClientResponse, error) {
	return c.DoRequest("POST", url, data...)
}

// Delete send DELETE request and returns the response object.
// Note that the response object MUST be closed if it'll be never used.
func (c *Client) Delete(url string, data ...interface{}) (*ClientResponse, error) {
	return c.DoRequest("DELETE", url, data...)
}

// Head send HEAD request and returns the response object.
// Note that the response object MUST be closed if it'll be never used.
func (c *Client) Head(url string, data ...interface{}) (*ClientResponse, error) {
	return c.DoRequest("HEAD", url, data...)
}

// Patch send PATCH request and returns the response object.
// Note that the response object MUST be closed if it'll be never used.
func (c *Client) Patch(url string, data ...interface{}) (*ClientResponse, error) {
	return c.DoRequest("PATCH", url, data...)
}

// Connect send CONNECT request and returns the response object.
// Note that the response object MUST be closed if it'll be never used.
func (c *Client) Connect(url string, data ...interface{}) (*ClientResponse, error) {
	return c.DoRequest("CONNECT", url, data...)
}

// Options send OPTIONS request and returns the response object.
// Note that the response object MUST be closed if it'll be never used.
func (c *Client) Options(url string, data ...interface{}) (*ClientResponse, error) {
	return c.DoRequest("OPTIONS", url, data...)
}

// Trace send TRACE request and returns the response object.
// Note that the response object MUST be closed if it'll be never used.
func (c *Client) Trace(url string, data ...interface{}) (*ClientResponse, error) {
	return c.DoRequest("TRACE", url, data...)
}

// DoRequest sends request with given HTTP method and data and returns the response object.
// Note that the response object MUST be closed if it'll be never used.
//
// Note that it uses "multipart/form-data" as its Content-Type if it contains file uploading,
// else it uses "application/x-www-form-urlencoded". It also automatically detects the post
// content for JSON format, and for that it automatically sets the Content-Type as
// "application/json".
func (c *Client) DoRequest(method, url string, data ...interface{}) (resp *ClientResponse, err error) {
	req, err := c.prepareRequest(method, url, data...)
	if err != nil {
		return nil, err
	}

	// Tracing.
	tr := otel.GetTracerProvider().Tracer(
		"github.com/gogf/gf/net/ghttp.client",
		trace.WithInstrumentationVersion(fmt.Sprintf(`%s`, gf.VERSION)),
	)
	ctx, span := tr.Start(req.Context(), req.URL.String())
	defer span.End()
	// Header (Cookie is in it).
	if len(req.Header) > 0 {
		span.SetAttributes(label.Any(`http.headers`, req.Header))
	}
	req = req.WithContext(ctx)

	// Client middleware.
	if len(c.middlewareHandler) > 0 {
		mdlHandlers := make([]ClientHandlerFunc, 0, len(c.middlewareHandler)+1)
		mdlHandlers = append(mdlHandlers, c.middlewareHandler...)
		mdlHandlers = append(mdlHandlers, func(cli *Client, r *http.Request) (*ClientResponse, error) {
			return cli.callRequest(r)
		})
		ctx := context.WithValue(req.Context(), clientMiddlewareKey, &clientMiddleware{
			client:       c,
			handlers:     mdlHandlers,
			handlerIndex: -1,
		})
		req = req.WithContext(ctx)
		resp, err = c.MiddlewareNext(req)
	} else {
		resp, err = c.callRequest(req)
	}

	// Auto saving cookie content.
	if c.browserMode && resp != nil {
		now := time.Now()
		for _, v := range resp.Response.Cookies() {
			if !v.Expires.IsZero() && v.Expires.UnixNano() < now.UnixNano() {
				delete(c.cookies, v.Name)
			} else {
				c.cookies[v.Name] = v.Value
			}
		}
	}
	return resp, err
}

// prepareRequest verifies request parameters, builds and returns http request.
func (c *Client) prepareRequest(method, url string, data ...interface{}) (req *http.Request, err error) {
	method = strings.ToUpper(method)
	if len(c.prefix) > 0 {
		url = c.prefix + gstr.Trim(url)
	}
	param := ""
	if len(data) > 0 {
		switch c.header["Content-Type"] {
		case "application/json":
			switch data[0].(type) {
			case string, []byte:
				param = gconv.String(data[0])
			default:
				if b, err := json.Marshal(data[0]); err != nil {
					return nil, err
				} else {
					param = gconv.UnsafeBytesToStr(b)
				}
			}
		case "application/xml":
			switch data[0].(type) {
			case string, []byte:
				param = gconv.String(data[0])
			default:
				if b, err := gparser.VarToXml(data[0]); err != nil {
					return nil, err
				} else {
					param = gconv.UnsafeBytesToStr(b)
				}
			}
		default:
			param = BuildParams(data[0])
		}
	}
	if method == "GET" {
		// It appends the parameters to the url if http method is GET.
		if param != "" {
			if gstr.Contains(url, "?") {
				url = url + "&" + param
			} else {
				url = url + "?" + param
			}
		}
		if req, err = http.NewRequest(method, url, bytes.NewBuffer(nil)); err != nil {
			return nil, err
		}
	} else {
		if strings.Contains(param, "@file:") {
			// File uploading request.
			buffer := new(bytes.Buffer)
			writer := multipart.NewWriter(buffer)
			for _, item := range strings.Split(param, "&") {
				array := strings.Split(item, "=")
				if len(array[1]) > 6 && strings.Compare(array[1][0:6], "@file:") == 0 {
					path := array[1][6:]
					if !gfile.Exists(path) {
						return nil, errors.New(fmt.Sprintf(`"%s" does not exist`, path))
					}
					if file, err := writer.CreateFormFile(array[0], gfile.Basename(path)); err == nil {
						if f, err := os.Open(path); err == nil {
							if _, err = io.Copy(file, f); err != nil {
								if err := f.Close(); err != nil {
									intlog.Errorf(`%+v`, err)
								}
								return nil, err
							}
							if err := f.Close(); err != nil {
								intlog.Errorf(`%+v`, err)
							}
						} else {
							return nil, err
						}
					} else {
						return nil, err
					}
				} else {
					if err = writer.WriteField(array[0], array[1]); err != nil {
						return nil, err
					}
				}
			}
			// Close finishes the multipart message and writes the trailing
			// boundary end line to the output.
			if err = writer.Close(); err != nil {
				return nil, err
			}

			if req, err = http.NewRequest(method, url, buffer); err != nil {
				return nil, err
			} else {
				req.Header.Set("Content-Type", writer.FormDataContentType())
			}
		} else {
			// Normal request.
			paramBytes := []byte(param)
			if req, err = http.NewRequest(method, url, bytes.NewReader(paramBytes)); err != nil {
				return nil, err
			} else {
				if v, ok := c.header["Content-Type"]; ok {
					// Custom Content-Type.
					req.Header.Set("Content-Type", v)
				} else if len(paramBytes) > 0 {
					if (paramBytes[0] == '[' || paramBytes[0] == '{') && json.Valid(paramBytes) {
						// Auto detecting and setting the post content format: JSON.
						req.Header.Set("Content-Type", "application/json")
					} else if gregex.IsMatchString(`^[\w\[\]]+=.+`, param) {
						// If the parameters passed like "name=value", it then uses form type.
						req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					}
				}
			}
		}
	}

	// Context.
	if c.ctx != nil {
		req = req.WithContext(c.ctx)
	} else {
		req = req.WithContext(context.Background())
	}
	// Custom header.
	if len(c.header) > 0 {
		for k, v := range c.header {
			req.Header.Set(k, v)
		}
	}
	// It's necessary set the req.Host if you want to custom the host value of the request.
	// It uses the "Host" value from header if it's not set in the request.
	if host := req.Header.Get("Host"); host != "" && req.Host == "" {
		req.Host = host
	}
	// Custom Cookie.
	if len(c.cookies) > 0 {
		headerCookie := ""
		for k, v := range c.cookies {
			if len(headerCookie) > 0 {
				headerCookie += ";"
			}
			headerCookie += k + "=" + v
		}
		if len(headerCookie) > 0 {
			req.Header.Set("Cookie", headerCookie)
		}
	}
	// HTTP basic authentication.
	if len(c.authUser) > 0 {
		req.SetBasicAuth(c.authUser, c.authPass)
	}
	// Client agent.
	if c.agent != "" {
		req.Header.Set("User-Agent", c.agent)
	}
	return req, nil
}

// callRequest sends request with give http.Request, and returns the responses object.
// Note that the response object MUST be closed if it'll be never used.
func (c *Client) callRequest(req *http.Request) (resp *ClientResponse, err error) {
	resp = &ClientResponse{
		request: req,
	}
	// The request body can be reused for dumping
	// raw HTTP request-response procedure.
	reqBodyContent, _ := ioutil.ReadAll(req.Body)
	resp.requestBody = reqBodyContent
	req.Body = utils.NewReadCloser(reqBodyContent, false)
	for {
		if resp.Response, err = c.Do(req); err != nil {
			// The response might not be nil when err != nil.
			if resp.Response != nil {
				if err := resp.Response.Body.Close(); err != nil {
					intlog.Errorf(`%+v`, err)
				}
			}
			if c.retryCount > 0 {
				c.retryCount--
				time.Sleep(c.retryInterval)
			} else {
				//return resp, err
				break
			}
		} else {
			break
		}
	}
	return resp, err
}
