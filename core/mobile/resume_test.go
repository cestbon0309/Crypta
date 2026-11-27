package mobile

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The core answers again after its listening socket is taken away.
func TestResume(t *testing.T) {
	dir := t.TempDir()
	out, err := Start(dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	get := func(out string) int {
		var info map[string]string
		json.Unmarshal([]byte(out), &info)
		req, _ := http.NewRequest("GET", info["url"]+"/api/setup", nil)
		req.Header.Set("Authorization", "Bearer "+info["token"])
		res, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
		if err != nil {
			return 0
		}
		res.Body.Close()
		return res.StatusCode
	}
	if again, err := Resume(); err != nil || again != out || get(out) != 200 {
		t.Fatal(again, err)
	}
	listener.Close()
	if get(out) != 0 {
		t.Fatal("still answering")
	}
	again, err := Resume()
	if err != nil || again != out || get(again) != 200 {
		t.Fatal(again, err, get(again))
	}
}
