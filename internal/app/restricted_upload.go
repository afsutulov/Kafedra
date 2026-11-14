package app

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"strings"
)

func validateRestrictedUpload(fh *multipart.FileHeader, ext string) error {
	f, err := fh.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	switch ext {
	case "jpg", "jpeg", "png":
		cfg, format, err := image.DecodeConfig(f)
		expected := "jpeg"
		if ext == "png" {
			expected = "png"
		}
		if err != nil || format != expected || cfg.Width < 1 || cfg.Height < 1 ||
			int64(cfg.Width)*int64(cfg.Height) > 24_000_000 {
			return errors.New("ожидается корректное изображение заявленного формата до 24 мегапикселей")
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, _, err := image.Decode(f); err != nil {
			return errors.New("изображение повреждено")
		}
	case "pdf":
		var header [8]byte
		if _, err := io.ReadFull(f, header[:]); err != nil || !bytes.HasPrefix(header[:], []byte("%PDF-")) {
			return errors.New("не найден заголовок PDF")
		}
		tail := make([]byte, min(fh.Size, 2048))
		// Сканеры и почтовые программы нередко дописывают после %%EOF нулевые байты или служебные строки,
		// поэтому маркер ищется в хвосте файла — так же поступают программы просмотра PDF.
		if _, err := f.ReadAt(tail, fh.Size-int64(len(tail))); (err != nil && err != io.EOF) || !bytes.Contains(tail, []byte("%%EOF")) {
			return errors.New("PDF повреждён или загружен не полностью")
		}
	case "docx":
		z, err := zip.NewReader(f, fh.Size)
		if err != nil {
			return errors.New("ожидается документ Word DOCX")
		}
		var types *zip.File
		hasDocument := false
		for _, entry := range z.File {
			name := strings.ToLower(entry.Name)
			if strings.Contains(name, "vbaproject") || strings.HasPrefix(name, "word/embeddings/") || strings.HasPrefix(name, "word/activex/") {
				return errors.New("DOCX с макросами или вложенными объектами запрещён")
			}
			switch entry.Name {
			case "[Content_Types].xml":
				types = entry
			case "word/document.xml":
				hasDocument = true
			}
		}
		if types == nil || !hasDocument || types.UncompressedSize64 > 1<<20 {
			return errors.New("в архиве отсутствует структура DOCX")
		}
		rc, err := types.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		var manifest struct {
			Overrides []struct {
				Name string `xml:"PartName,attr"`
				Type string `xml:"ContentType,attr"`
			} `xml:"Override"`
		}
		if err := xml.NewDecoder(io.LimitReader(rc, (1<<20)+1)).Decode(&manifest); err != nil {
			return errors.New("некорректный манифест DOCX")
		}
		for _, item := range manifest.Overrides {
			if item.Name == "/word/document.xml" && item.Type == "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml" {
				return nil
			}
		}
		return errors.New("ожидается документ DOCX без макросов")
	}
	return nil
}
