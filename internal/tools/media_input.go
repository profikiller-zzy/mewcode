package tools

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/gif"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
)

// inputImageRawBudget 是我们允许塞进 data URI 的最大 JPEG 字节数，
// 用于 img2img。Agnes 接受的 data URI 总长上限约 80KB，而 base64 会把
// 载荷撑大 33% 左右，所以把原始 JPEG 卡在 50KB，
// 最终的 URI 就能稳稳低于那个阈值。
const inputImageRawBudget = 50 * 1024

// prepareInputImage 把 input_images 里的一项规范化后交给上游 API。
//
//   - http(s):// 开头的 URL 原样放过 —— 供应商自己会去取，
//     对 URL 也没什么有意义的大小上限可卡。
//   - data: URI 和本地文件路径会先解码、降采样，再重新编码成
//     JPEG 直到塞进 inputImageRawBudget，最后以 data URI 返回。
//
// workDir 用来解析相对路径。文件读不到或图片解不出来时
// 返回 ("", err)。
func prepareInputImage(in, workDir string) (string, error) {
	if in == "" {
		return "", errors.New("empty input image")
	}
	low := strings.ToLower(in)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return in, nil
	}

	var raw []byte
	switch {
	case strings.HasPrefix(low, "data:"):
		b, err := decodeDataURI(in)
		if err != nil {
			return "", err
		}
		raw = b
	default:
		path := in
		if !filepath.IsAbs(path) && workDir != "" {
			path = filepath.Join(workDir, path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read input image %q: %w", in, err)
		}
		raw = b
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("decode input image: %w", err)
	}

	jpegBytes, err := shrinkToBudget(img, inputImageRawBudget)
	if err != nil {
		return "", err
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegBytes), nil
}

// shrinkToBudget 把 img 编码成 JPEG：先降质量，再把尺寸减半，
// 直到编码后的字节数 <= budget。返回我们能产出的最小编码结果；
// 只有当连 8x8 缩略图都塞不下时才报错
// （那意味着 budget 小得离谱）。
func shrinkToBudget(img image.Image, budget int) ([]byte, error) {
	qualities := []int{82, 65, 45, 28}
	for {
		for _, q := range qualities {
			buf := &bytes.Buffer{}
			if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: q}); err != nil {
				return nil, fmt.Errorf("encode jpeg: %w", err)
			}
			if buf.Len() <= budget {
				return buf.Bytes(), nil
			}
		}
		b := img.Bounds()
		w, h := b.Dx()/2, b.Dy()/2
		if w < 8 || h < 8 {
			return nil, fmt.Errorf("cannot shrink image under %d bytes", budget)
		}
		img = halve(img)
	}
}

// halve 用 2x2 盒式平均把 img 缩到宽高各一半。
// 盒式滤波开销小、降采样干净 —— 反正结果要喂给 img2img 模型，
// 不需要精确还原。
func halve(src image.Image) image.Image {
	sb := src.Bounds()
	w, h := sb.Dx()/2, sb.Dy()/2
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sx, sy := sb.Min.X+x*2, sb.Min.Y+y*2
			r1, g1, b1, a1 := src.At(sx, sy).RGBA()
			r2, g2, b2, a2 := src.At(sx+1, sy).RGBA()
			r3, g3, b3, a3 := src.At(sx, sy+1).RGBA()
			r4, g4, b4, a4 := src.At(sx+1, sy+1).RGBA()
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8((r1 + r2 + r3 + r4) >> 10),
				G: uint8((g1 + g2 + g3 + g4) >> 10),
				B: uint8((b1 + b2 + b3 + b4) >> 10),
				A: uint8((a1 + a2 + a3 + a4) >> 10),
			})
		}
	}
	return dst
}

// validateVideoInputURL 要求 generate_video 的输入图必须是公开的
// http(s) URL。Agnes 的视频端点只接受 URL —— 本地路径和
// data URI 会绊到它的 base64 / fetch 处理逻辑，任务会异步失败并
// 抛出一个让人摸不着头脑的 "Invalid image" 错误，以前调用方
// 看到的就是一遍遍通用的 "parse error" 重试。
func validateVideoInputURL(in string) error {
	if in == "" {
		return errors.New("empty input image")
	}
	low := strings.ToLower(in)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return nil
	}
	if strings.HasPrefix(low, "data:") {
		return fmt.Errorf("video input_images must be public http(s) URLs; data URIs are not accepted by the upstream API")
	}
	return fmt.Errorf("video input_images must be public http(s) URLs (got %q); upload the image to a reachable URL first", in)
}

// decodeDataURI 从 "data:[mime];base64,..." 字符串里取出原始字节。
// 非 base64 的 data URI 一律拒绝，因为 API 只往返 base64。
func decodeDataURI(s string) ([]byte, error) {
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return nil, errors.New("data URI missing comma separator")
	}
	header := s[:comma]
	if !strings.Contains(header, ";base64") {
		return nil, errors.New("data URI must be base64-encoded")
	}
	return base64.StdEncoding.DecodeString(s[comma+1:])
}
