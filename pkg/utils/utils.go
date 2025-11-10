package utils

import (
	"cmp"
	"crypto/rand"
	"errors"
	"fmt"
	"iter"
	"maps"
	"math/big"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/exp/constraints"
)

var (
	DNSServersV4 = []string{"8.8.8.8:53", "8.8.4.4:53", "1.1.1.1:53", "1.0.0.1:53"}
	DNSServersV6 = []string{"[2001:4860:4860::8888]:53", "[2001:4860:4860::8844]:53", "[2606:4700:4700::1111]:53", "[2606:4700:4700::1001]:53"}
	DNSServers   = append(DNSServersV4, DNSServersV6...)

	ipv4Re = regexp.MustCompile(`(\d*\.).*(\.\d*)`)
	ipv6Re = regexp.MustCompile(`(\w*:\w*:).*(:\w*:\w*)`)
)

func ipv4Desensitize(ipv4Addr string) string {
	return ipv4Re.ReplaceAllString(ipv4Addr, "$1****$2")
}

func ipv6Desensitize(ipv6Addr string) string {
	return ipv6Re.ReplaceAllString(ipv6Addr, "$1****$2")
}

func IPDesensitize(ipAddr string) string {
	ipAddr = ipv4Desensitize(ipAddr)
	ipAddr = ipv6Desensitize(ipAddr)
	return ipAddr
}

func IPStringToBinary(ip string) ([]byte, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return nil, err
	}
	b := addr.As16()
	return b[:], nil
}

func BinaryToIPString(b []byte) string {
	if len(b) < 16 {
		return "::"
	}

	addr := netip.AddrFrom16([16]byte(b))
	return addr.Unmap().String()
}

func GetIPFromHeader(headerValue string) (string, error) {
	a := strings.Split(headerValue, ",")
	h := strings.TrimSpace(a[len(a)-1])
	ip, err := netip.ParseAddr(h)
	if err != nil {
		return "", err
	}
	if !ip.IsValid() {
		return "", errors.New("invalid ip")
	}
	return ip.String(), nil
}

func GenerateRandomString(n int) (string, error) {
	const letters = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	lettersLength := big.NewInt(int64(len(letters)))
	ret := make([]byte, n)
	for i := range n {
		num, err := rand.Int(rand.Reader, lettersLength)
		if err != nil {
			return "", err
		}
		ret[i] = letters[num.Int64()]
	}
	return string(ret), nil
}

func MustGenerateRandomString(n int) string {
	str, err := GenerateRandomString(n)
	if err != nil {
		panic(fmt.Errorf("MustGenerateRandomString: %v", err))
	}
	return str
}

func IfOr[T any](a bool, x, y T) T {
	if a {
		return x
	}
	return y
}

func Itoa[T constraints.Integer](i T) string {
	switch any(i).(type) {
	case int, int8, int16, int32, int64:
		return strconv.FormatInt(int64(i), 10)
	case uint, uint8, uint16, uint32, uint64:
		return strconv.FormatUint(uint64(i), 10)
	default:
		return ""
	}
}

func MapValuesToSlice[Map ~map[K]V, K comparable, V any](m Map) []V {
	s := make([]V, 0, len(m))
	return slices.AppendSeq(s, maps.Values(m))
}

func MapKeysToSlice[Map ~map[K]V, K comparable, V any](m Map) []K {
	s := make([]K, 0, len(m))
	return slices.AppendSeq(s, maps.Keys(m))
}

func Unique[S ~[]E, E cmp.Ordered](list S) S {
	if list == nil {
		return nil
	}
	out := make([]E, len(list))
	copy(out, list)
	slices.Sort(out)
	return slices.Compact(out)
}

func ConvertSeq[In, Out any](seq iter.Seq[In], f func(In) Out) iter.Seq[Out] {
	return func(yield func(Out) bool) {
		for in := range seq {
			if !yield(f(in)) {
				return
			}
		}
	}
}

func ConvertSeq2[KIn, VIn, KOut, VOut any](seq iter.Seq2[KIn, VIn], f func(KIn, VIn) (KOut, VOut)) iter.Seq2[KOut, VOut] {
	return func(yield func(KOut, VOut) bool) {
		for k, v := range seq {
			if !yield(f(k, v)) {
				return
			}
		}
	}
}

func Seq2To1[K, V any](seq iter.Seq2[K, V]) iter.Seq[V] {
	return func(yield func(V) bool) {
		for _, v := range seq {
			if !yield(v) {
				return
			}
		}
	}
}

type WrapError struct {
	err, errIn error
}

func NewWrapError(err, errIn error) error {
	return &WrapError{err, errIn}
}

func (e *WrapError) Error() string {
	return e.err.Error()
}

func (e *WrapError) Unwrap() error {
	return e.errIn
}

func FirstError(errorer ...func() error) error {
	for _, fn := range errorer {
		if err := fn(); err != nil {
			return err
		}
	}
	return nil
}

func SubUintChecked[T constraints.Unsigned](a, b T) T {
	if a < b {
		return 0
	}

	return a - b
}
