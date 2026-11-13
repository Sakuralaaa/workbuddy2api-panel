package upstream

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
)

// readTestBody 读出站请求体：official 形态的 chat 请求体带 Content-Encoding: gzip
// （对齐插件 gzipped()），测试桩按上游的方式解压后再断言。
func readTestBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil || r.Header.Get("Content-Encoding") != "gzip" {
		return raw, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return raw, err
	}
	return io.ReadAll(zr)
}
