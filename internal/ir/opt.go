package ir

// Optimize removes dead pure instructions and compacts variable/array numbering.
func Optimize(p *Program) {
	for {
		used := map[int]bool{}
		usedArr := map[int]bool{}
		collectReads(p.Body, used, usedArr)
		changed := false
		p.Body = dce(p.Body, used, usedArr, &changed)
		if !changed {
			break
		}
	}
	compact(p)
}

func rd(o Operand, used map[int]bool) {
	if !o.Const {
		used[o.Var] = true
	}
}

func collectReads(b []Inst, u map[int]bool, ua map[int]bool) {
	for _, in := range b {
		switch s := in.(type) {
		case Mov:
			rd(s.A, u)
		case Bin:
			rd(s.A, u)
			rd(s.B, u)
		case Neg:
			rd(s.A, u)
		case Not:
			rd(s.A, u)
		case Cast:
			rd(s.A, u)
		case Clamp:
			rd(s.A, u)
			rd(s.Lo, u)
			rd(s.Hi, u)
		case Load:
			rd(s.Idx, u)
			ua[s.Arr] = true
		case Store:
			rd(s.Idx, u)
			rd(s.A, u)
		case ArrInit:
			for _, v := range s.Vals {
				rd(v, u)
			}
		case FBin:
			rd(s.A, u)
			rd(s.B, u)
		case FUn:
			rd(s.A, u)
		case FCvt:
			rd(s.A, u)
		case WriteFloat:
			rd(s.A, u)
		case StrBuild:
			for _, q := range s.Parts {
				rd(q.Len, u)
				rd(q.A, u)
			}
		case BufEq:
			rd(s.LenA, u)
			rd(s.LenB, u)
		case TLoad:
			rd(s.Idx, u)
			ua[-(s.Tab + 1)] = true
		case BufLoad:
			rd(s.Idx, u)
			rd(s.Len, u)
		case StrEq:
			rd(s.Len, u)
		case ArrFill:
			rd(s.Val, u)
		case ArrCopy:
			ua[s.Src] = true
		case WriteBuf:
			rd(s.Len, u)
		case WriteDec:
			rd(s.A, u)
		case Alloc:
			rd(s.Size, u)
		case Reset:
			u[s.Slot] = true
		case If:
			rd(s.Cond, u)
			collectReads(s.Then, u, ua)
			collectReads(s.Else, u, ua)
		case While:
			rd(s.Cond, u)
			collectReads(s.Pre, u, ua)
			collectReads(s.Body, u, ua)
		case Ret:
			if s.Has {
				rd(s.A, u)
			}
		}
	}
}

func dce(b []Inst, u map[int]bool, ua map[int]bool, changed *bool) []Inst {
	var out []Inst
	drop := func() { *changed = true }
	for _, in := range b {
		switch s := in.(type) {
		case Mov:
			if !u[s.Dst] || (!s.A.Const && s.A.Var == s.Dst) {
				drop()
				continue
			}
		case Bin:
			if !s.Check && !u[s.Dst] {
				drop()
				continue
			}
		case Neg:
			if !s.Check && !u[s.Dst] {
				drop()
				continue
			}
		case Not:
			if !u[s.Dst] {
				drop()
				continue
			}
		case Clamp:
			if !s.TrapMode && !u[s.Dst] {
				drop()
				continue
			}
		case Load:
			if !s.Check && !u[s.Dst] {
				drop()
				continue
			}
		case TLoad:
			if !s.Check && !u[s.Dst] {
				drop()
				continue
			}
		case FBin:
			if !u[s.Dst] {
				drop()
				continue
			}
		case FUn:
			if !u[s.Dst] {
				drop()
				continue
			}
		case FCvt:
			if s.Kind != CvtFloatToInt && !u[s.Dst] { // float->int can trap and must stay
				drop()
				continue
			}
		case StrEq:
			if !u[s.Dst] {
				drop()
				continue
			}
		case BufEq:
			if !u[s.Dst] {
				drop()
				continue
			}
		case Store:
			if !s.Check && !ua[s.Arr] {
				drop()
				continue
			}
		case ArrInit:
			if !ua[s.Arr] {
				drop()
				continue
			}
		case ArrFill:
			if !ua[s.Arr] {
				drop()
				continue
			}
		case ArrCopy:
			if !ua[s.Dst] {
				drop()
				continue
			}
		case If:
			s.Then = dce(s.Then, u, ua, changed)
			s.Else = dce(s.Else, u, ua, changed)
			if len(s.Then) == 0 && len(s.Else) == 0 {
				drop()
				continue
			}
			if len(s.Then) == 0 {
				// if c {} else {X}  ==>  keep as is (rare); fine
			}
			in = s
		case While:
			s.Pre = dce(s.Pre, u, ua, changed)
			s.Body = dce(s.Body, u, ua, changed)
			in = s
		}
		out = append(out, in)
	}
	return out
}

type remap struct {
	vars, arrs, tabs map[int]int
}

func (r *remap) t(id int) int {
	if n, ok := r.tabs[id]; ok {
		return n
	}
	n := len(r.tabs)
	r.tabs[id] = n
	return n
}

func (r *remap) v(id int) int {
	if n, ok := r.vars[id]; ok {
		return n
	}
	n := len(r.vars)
	r.vars[id] = n
	return n
}
func (r *remap) a(id int) int {
	if n, ok := r.arrs[id]; ok {
		return n
	}
	n := len(r.arrs)
	r.arrs[id] = n
	return n
}
func (r *remap) o(o Operand) Operand {
	if o.Const {
		return o
	}
	return Operand{Var: r.v(o.Var)}
}

func (r *remap) block(b []Inst) []Inst {
	out := make([]Inst, len(b))
	for i, in := range b {
		switch s := in.(type) {
		case Mov:
			s.Dst, s.A = r.v(s.Dst), r.o(s.A)
			in = s
		case Bin:
			s.A, s.B = r.o(s.A), r.o(s.B)
			s.Dst = r.v(s.Dst)
			in = s
		case Neg:
			s.A = r.o(s.A)
			s.Dst = r.v(s.Dst)
			in = s
		case Not:
			s.A = r.o(s.A)
			s.Dst = r.v(s.Dst)
			in = s
		case Cast:
			s.A = r.o(s.A)
			s.Dst = r.v(s.Dst)
			in = s
		case Clamp:
			s.A, s.Lo, s.Hi = r.o(s.A), r.o(s.Lo), r.o(s.Hi)
			s.Dst = r.v(s.Dst)
			in = s
		case Load:
			s.Idx = r.o(s.Idx)
			s.Dst, s.Arr = r.v(s.Dst), r.a(s.Arr)
			in = s
		case Store:
			s.Idx, s.A = r.o(s.Idx), r.o(s.A)
			s.Arr = r.a(s.Arr)
			in = s
		case ArrInit:
			vs := make([]Operand, len(s.Vals))
			for j, v := range s.Vals {
				vs[j] = r.o(v)
			}
			s.Vals, s.Arr = vs, r.a(s.Arr)
			in = s
		case FBin:
			s.A, s.B = r.o(s.A), r.o(s.B)
			s.Dst = r.v(s.Dst)
			in = s
		case FUn:
			s.A = r.o(s.A)
			s.Dst = r.v(s.Dst)
			in = s
		case FCvt:
			s.A = r.o(s.A)
			s.Dst = r.v(s.Dst)
			in = s
		case WriteFloat:
			s.A = r.o(s.A)
			in = s
		case ReadFloat:
			s.Dst = r.v(s.Dst)
			in = s
		case StrBuild:
			ps := make([]StrPart, len(s.Parts))
			for j, q := range s.Parts {
				q.Len, q.A = r.o(q.Len), r.o(q.A)
				ps[j] = q
			}
			s.Parts = ps
			s.Len = r.v(s.Len)
			in = s
		case BufEq:
			s.LenA, s.LenB = r.o(s.LenA), r.o(s.LenB)
			s.Dst = r.v(s.Dst)
			in = s
		case TLoad:
			s.Idx = r.o(s.Idx)
			s.Dst, s.Tab = r.v(s.Dst), r.t(s.Tab)
			in = s
		case BufLoad:
			s.Idx, s.Len = r.o(s.Idx), r.o(s.Len)
			s.Dst = r.v(s.Dst)
			in = s
		case StrEq:
			s.Len = r.o(s.Len)
			s.Dst = r.v(s.Dst)
			in = s
		case ArrFill:
			s.Val = r.o(s.Val)
			s.Arr = r.a(s.Arr)
			in = s
		case ArrCopy:
			s.Src = r.a(s.Src)
			s.Dst = r.a(s.Dst)
			in = s
		case ReadLine:
			s.Len = r.v(s.Len)
			in = s
		case ReadInt:
			s.Dst = r.v(s.Dst)
			in = s
		case WriteBuf:
			s.Len = r.o(s.Len)
			in = s
		case WriteDec:
			s.A = r.o(s.A)
			in = s
		case Alloc:
			s.Size = r.o(s.Size)
			s.Dst = r.v(s.Dst)
			in = s
		case Mark:
			s.Slot = r.v(s.Slot)
			in = s
		case Reset:
			s.Slot = r.v(s.Slot)
			in = s
		case If:
			s.Cond = r.o(s.Cond)
			s.Then, s.Else = r.block(s.Then), r.block(s.Else)
			in = s
		case While:
			s.Pre = r.block(s.Pre)
			s.Cond = r.o(s.Cond)
			s.Body = r.block(s.Body)
			in = s
		case Ret:
			if s.Has {
				s.A = r.o(s.A)
			}
			in = s
		}
		out[i] = in
	}
	return out
}

// compact renumbers variables and arrays densely in order of appearance.
func compact(p *Program) {
	r := &remap{vars: map[int]int{}, arrs: map[int]int{}, tabs: map[int]int{}}
	body := r.block(p.Body)
	p.Body = body
	p.NVars = len(r.vars)
	arrs := make([]ArrInfo, len(r.arrs))
	for old, nw := range r.arrs {
		arrs[nw] = p.Arrs[old]
	}
	p.Arrs = arrs
	tabs := make([]Table, len(r.tabs))
	for old, nw := range r.tabs {
		tabs[nw] = p.Tables[old]
	}
	p.Tables = tabs
}
