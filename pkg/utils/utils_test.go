package utils

import (
	"reflect"
	"testing"
)

type testSt struct {
	input  string
	output string
}

func TestNotification(t *testing.T) {
	cases := []testSt{
		{
			input:  "103.80.236.249/d5ce:d811:cdb8:067a:a873:2076:9521:9d2d",
			output: "103.****.249/d5ce:d811:****:9521:9d2d",
		},
		{
			input:  "3.80.236.29/d5ce::cdb8:067a:a873:2076:9521:9d2d",
			output: "3.****.29/d5ce::****:9521:9d2d",
		},
		{
			input:  "3.80.236.29/d5ce::cdb8:067a:a873:2076::9d2d",
			output: "3.****.29/d5ce::****::9d2d",
		},
		{
			input:  "3.80.236.9/d5ce::cdb8:067a:a873:2076::9d2d",
			output: "3.****.9/d5ce::****::9d2d",
		},
		{
			input:  "3.80.236.9/d5ce::cdb8:067a:a873:2076::9d2d",
			output: "3.****.9/d5ce::****::9d2d",
		},
	}

	for _, c := range cases {
		if c.output != IPDesensitize(c.input) {
			t.Fatalf("Expected %s, but got %s", c.output, IPDesensitize(c.input))
		}
	}
}

func TestGenerGenerateRandomString(t *testing.T) {
	generatedString := make(map[string]bool)
	for range 100 {
		str, err := GenerateRandomString(32)
		if err != nil {
			t.Fatalf("Error: %s", err)
		}
		if len(str) != 32 {
			t.Fatalf("Expected 32, but got %d", len(str))
		}
		if generatedString[str] {
			t.Fatalf("Duplicated string: %s", str)
		}
		generatedString[str] = true
	}
}

func TestIPStringToBinary(t *testing.T) {
	cases := []struct {
		ip          string
		want        []byte
		expectError bool
	}{
		// 有效的 IPv4 地址
		{
			ip: "192.168.1.1",
			want: []byte{
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255, 192, 168, 1, 1,
			},
			expectError: false,
		},
		// 有效的 IPv6 地址
		{
			ip: "2001:db8::68",
			want: []byte{
				32, 1, 13, 184, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 104,
			},
			expectError: false,
		},
		// 无效的 IP 地址
		{
			ip:          "invalid_ip",
			want:        []byte{},
			expectError: true,
		},
	}

	for _, c := range cases {
		got, err := IPStringToBinary(c.ip)
		if (err != nil) != c.expectError {
			t.Errorf("IPStringToBinary(%q) error = %v, expect error = %v", c.ip, err, c.expectError)
			continue
		}
		if err == nil && !reflect.DeepEqual(got, c.want) {
			t.Errorf("IPStringToBinary(%q) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestBinaryToIPString(t *testing.T) {
	cases := []struct {
		binary []byte
		want   string
	}{
		// IPv4 地址（IPv4 映射的 IPv6 地址格式）
		{
			binary: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255, 192, 168, 1, 1},
			want:   "192.168.1.1",
		},
		// 其他测试用例
		{
			binary: []byte{32, 1, 13, 184, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 104},
			want:   "2001:db8::68",
		},
		// 全零值
		{
			binary: []byte{},
			want:   "::",
		},
		// IPv4 映射的 IPv6 地址
		{
			binary: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255, 127, 0, 0, 1},
			want:   "127.0.0.1",
		},
	}

	for _, c := range cases {
		got := BinaryToIPString(c.binary)
		if got != c.want {
			t.Errorf("BinaryToIPString(%v) = %q, 期望 %q", c.binary, got, c.want)
		}
	}
}

func TestUnique(t *testing.T) {
	cases := []struct {
		input  []string
		output []string
	}{
		{
			input:  []string{"a", "b", "c", "a", "b", "c"},
			output: []string{"a", "b", "c"},
		},
		{
			input:  []string{"a", "b", "c"},
			output: []string{"a", "b", "c"},
		},
		{
			input:  []string{"a", "a", "a"},
			output: []string{"a"},
		},
		{
			input:  []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"},
			output: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"},
		},
		{
			input:  []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "a"},
			output: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"},
		},
		{
			input:  []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "a", "b", "c", "d", "e", "f", "g", "h", "i"},
			output: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"},
		},
		{
			input:  []string{},
			output: []string{},
		},
		{
			input:  []string{"a"},
			output: []string{"a"},
		},
	}

	for _, c := range cases {
		if !reflect.DeepEqual(Unique(c.input), c.output) {
			t.Fatalf("Expected %v, but got %v", c.output, Unique(c.input))
		}
	}
}
