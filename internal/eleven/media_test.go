package eleven

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRef(t *testing.T) {
	r, err := Ref("generation:abc123")
	if err != nil || r["type"] != "generation" || r["generation_id"] != "abc123" {
		t.Fatalf("generation ref: %v %v", r, err)
	}
	r, err = Ref("asset:xyz")
	if err != nil || r["type"] != "asset" || r["asset_id"] != "xyz" {
		t.Fatalf("asset ref: %v %v", r, err)
	}
	dir := t.TempDir()
	png := filepath.Join(dir, "a.PNG")
	_ = os.WriteFile(png, []byte("fake"), 0o644)
	r, err = Ref(png)
	if err != nil || r["type"] != "inline_base64" || r["mime_type"] != "image/png" || r["content_base64"] != "ZmFrZQ==" {
		t.Fatalf("file ref: %v %v", r, err)
	}
	txt := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(txt, []byte("x"), 0o644)
	if _, err := Ref(txt); err == nil {
		t.Error("expected unsupported type error")
	}
	if _, err := Ref(filepath.Join(dir, "missing.png")); err == nil {
		t.Error("expected missing file error")
	}
}
