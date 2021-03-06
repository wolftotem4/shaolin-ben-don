package log

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
)

func SaveResponse(res *http.Response, content []byte, dir string) error {
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return errors.WithStack(err)
	}

	filename := fmt.Sprintf("%s_%s.log", time.Now().Format("20060102150405.999999"), url.QueryEscape(res.Request.URL.String()))
	file, err := os.Create(filepath.Join(dir, filename))
	if err != nil {
		return errors.WithStack(err)
	}
	defer file.Close()

	_, err = file.Write(content)
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}
