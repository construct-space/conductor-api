package main

import (
	"encoding/json"
	"net/http"
)

func jsonDecode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }
