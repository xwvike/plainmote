package web

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// A text file chosen in the browser is read as the encoding detected for it,
// and with encryption is saved as UTF-8 from that reading, so a wrong guess
// would be kept. This holds the detection to real-looking configuration and
// log text in the encodings people still have, from a dozen characters up -
// shorter than that, the page shows its guess and the list to correct it.
func TestEncodingDetection(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	corpus := []struct {
		name string
		enc  encoding.Encoding
		text string
	}{
		{"gb18030", simplifiedchinese.GB18030, "# 上海节点配置文件\nserver: sh-01.example.com\nport: 7890\n# 说明：此文件用于测试中文配置的编码识别，请勿删除。\n日志级别: 调试\n错误：无法连接到数据库，请检查网络设置后重试。\n"},
		{"gbk", simplifiedchinese.GBK, "名称: 临时节点\n备注: 北京电信线路，晚高峰可能拥堵\n用户名: 管理员\n密码长度不足，请重新输入。\n"},
		{"big5", traditionalchinese.Big5, "# 台北節點設定檔\n伺服器: tpe-01.example.com\n連接埠: 8080\n說明：此檔案用於測試繁體中文設定的編碼識別。\n錯誤：無法連線到資料庫，請檢查網路設定。\n"},
		{"shift_jis", japanese.ShiftJIS, "# 東京ノード設定ファイル\nサーバー: tyo-01.example.com\nポート: 443\n説明：このファイルは日本語の文字コード判定のテストに使用します。\nエラー：データベースに接続できません。\n"},
		{"euc-jp", japanese.EUCJP, "# 大阪ノード設定\n説明：文字コード判定のテストです。\nエラー：接続がタイムアウトしました。\n"},
		{"euc-kr", korean.EUCKR, "# 서울 노드 설정 파일\n서버: sel-01.example.com\n설명: 한국어 문자 인코딩 감지 테스트용 파일입니다.\n오류: 데이터베이스에 연결할 수 없습니다.\n"},
		{"windows-1251", charmap.Windows1251, "# Настройки узла Москва\nсервер: mow-01.example.com\nописание: файл для проверки определения кодировки.\nошибка: не удалось подключиться к базе данных.\n"},
		// Nothing from 0x80-0x9F, where Node, unlike every browser, decodes
		// windows-1252 as Latin-1; the browser reads those too.
		{"windows-1252", charmap.Windows1252, "# Paramètres du serveur de Paris\nserveur: par-01.example.com\ndescription: fichier de test pour la détection de l'encodage, déjà vérifié.\nerreur: impossible de se connecter à la base de données.\n"},
		{"utf-8", encoding.Nop, "plain ASCII config\nport=80\n"},
	}
	type sample struct {
		Name  string `json:"name"`
		Text  string `json:"text"`
		Bytes []byte `json:"bytes"`
	}
	var samples []sample
	for _, c := range corpus {
		runes := []rune(c.text)
		for _, n := range []int{12, 25, 50, 100, len(runes)} {
			text := string(runes[:min(n, len(runes))])
			encoded, err := c.enc.NewEncoder().Bytes([]byte(text))
			if err != nil {
				t.Fatal(c.name, err)
			}
			samples = append(samples, sample{c.name, text, encoded})
		}
	}
	input, _ := json.Marshal(samples)
	script, err := os.ReadFile(filepath.Join("testdata", "encoding_test.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	module, _ := filepath.Abs(filepath.Join("static", "editor.js"))
	command := exec.Command(node, "--input-type=module", "-e", string(script))
	command.Env = append(os.Environ(), "EDITOR_MODULE=file://"+module)
	command.Stdin = bytes.NewReader(input)
	out, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Encoding string  `json:"encoding"`
		Text     *string `json:"text"`
	}
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(samples) {
		t.Fatalf("output: %v %s", err, out)
	}
	for i, s := range samples {
		if got[i].Text == nil || *got[i].Text != s.Text {
			t.Errorf("%s, %d characters: detected %q, which does not read back the text", s.Name, len([]rune(s.Text)), got[i].Encoding)
		}
	}
}
