package types

import "testing"

func TestIntRanges(t *testing.T) {
	cases := []struct {
		name   string
		lo, hi string
	}{
		{"u8", "0", "255"}, {"i8", "-128", "127"}, {"u16", "0", "65535"}, {"i16", "-32768", "32767"},
		{"u32", "0", "4294967295"}, {"i32", "-2147483648", "2147483647"},
		{"u64", "0", "18446744073709551615"}, {"i64", "-9223372036854775808", "9223372036854775807"},
		{"usize", "0", "18446744073709551615"}, {"isize", "-9223372036854775808", "9223372036854775807"},
	}
	for _, c := range cases {
		lo, hi := Int(c.name).Range()
		if lo.String() != c.lo || hi.String() != c.hi {
			t.Errorf("%s: range = [%s,%s], want [%s,%s]", c.name, lo, hi, c.lo, c.hi)
		}
	}
}

func TestSizeAlign(t *testing.T) {
	packet := &Type{Kind: KStruct, Name: "Packet", Fields: []Field{{"id", Int("u16")}, {"payload", Array(U8, 512)}}}
	if packet.Size() != 514 || packet.Align() != 2 {
		t.Errorf("Packet size/align = %d/%d, want 514/2", packet.Size(), packet.Align())
	}
	padded := &Type{Kind: KStruct, Name: "P", Fields: []Field{{"a", U8}, {"b", U32}, {"c", U8}}}
	if padded.Size() != 12 || padded.Align() != 4 {
		t.Errorf("padded struct = %d/%d, want 12/4", padded.Size(), padded.Align())
	}
	if Str(10).Size() != 14 {
		t.Errorf("Str<10> size = %d, want 14", Str(10).Size())
	}
	if Array(Array(I32, 3), 3).Size() != 36 {
		t.Errorf("[[i32;3];3] size wrong")
	}
	if Bool.Size() != 1 || Ptr.Size() != 8 || Unit.Size() != 0 {
		t.Errorf("scalar sizes wrong")
	}
}

func TestEqualAssignable(t *testing.T) {
	if !Equal(I32, Int("i32")) || Equal(I32, U32) || Equal(U64, Int("usize")) == false {
		t.Error("integer equality wrong")
	}
	if !Equal(Array(U8, 4), Array(U8, 4)) || Equal(Array(U8, 4), Array(U8, 5)) {
		t.Error("array equality wrong")
	}
	if !Assignable(Str(3), Str(8)) || Assignable(Str(8), Str(3)) {
		t.Error("string capacity subtyping wrong")
	}
	if !Assignable(Untyped, U8) || Assignable(Untyped, Bool) {
		t.Error("untyped literal assignability wrong")
	}
	if Assignable(I32, I64) {
		t.Error("no implicit widening")
	}
}

func TestDecimalDigits(t *testing.T) {
	want := map[string]int{"u8": 3, "i8": 4, "u32": 10, "i32": 11, "u64": 20, "i64": 20}
	for n, d := range want {
		if got := Int(n).DecimalDigits(); got != d {
			t.Errorf("%s digits = %d, want %d", n, got, d)
		}
	}
}
