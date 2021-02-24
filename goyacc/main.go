// Copyright 2014 The goyacc Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Goyacc is a version of yacc generating Go parsers.
//
// Usage
//
// Note: If no non flag arguments are given, goyacc reads standard input.
//
//	goyacc [options] [input]
//
//	options and (defaults)
//		-c                  Report state closures. (false)
//		-cr                 Check all states are reducible. (false)
//		-dlval              Debug value when runtime yyDebug >= 3. ("lval")
//		-dlvalf             Debug format of -dlval. ("%+v")
//		-ex                 Explain how were conflicts resolved. (false)
//		-fs                 Emit follow sets. (false)
//		-l                  Disable line directives, for compatibility only - ignored. (false)
//		-la                 Report all lookahead sets. (false)
//		-o outputFile       Parser output. ("y.go")
//		-p prefix           Name prefix to use in generated code. ("yy")
//		-pool               Use sync.Pool for the parser stack
//		-v reportFile       Create grammar report. ("y.output")
//		-xe examplesFile    Generate error messages by examples. ("")
//		-xegen examplesFile Generate a file suitable for -xe automatically from the grammar.
//		                    The file must not exist. ("")
//
//
//
// Changelog
//
// 2018-03-23: The new option -pool enables using sync.Pool to recycle parser
// stacks.
//
// 2017-08-01: New option -fs emits a table of the follow sets. Index is the
// state number.
//
// 2016-03-17: Error messages now use the last token literal string, if any, to
// produce nicer text like "unexpected integer constant". If using xerrors the
// message could be, for example, something like "unexpected integer constant,
// expected '{'"-
//
// 2015-03-24: The search for a custom error message is now extended to include
// also the last state that was shifted into, if any. This change resolves a
// problem in which a lookahead symbol is valid for a reduce action in state A,
// but the same symbol is later never accepted by any shift action in some
// state B which is popped from the state stack after the reduction is
// performed. The computed from example state is A but when the error is
// actually detected, the state is now B and the custom error was thus not
// used.
//
// 2015-02-23: Added -xegen flag. It can be used to automagically generate a
// skeleton errors by example file which can be, for example, edited and/or
// submited later as an argument of the -xe option.
//
// 2014-12-18: Support %precedence for better bison compatibility[3]. The
// actual changes are in packages goyacc is dependent on. Goyacc users should
// rebuild the binary:
//
//	$ go get -u github.com/cznic/goyacc
//
// 2014-12-02: Added support for the optional yyLexerEx interface. The Reduced
// method can be useful for debugging and/or automatically producing examples
// by parsing code fragments. If it returns true the parser exits immediately
// with return value -1.
//
// Overview
//
// The generated parser is reentrant and mostly backwards compatible with
// parsers generated by go tool yacc[0]. yyParse expects to be given an
// argument that conforms to the following interface:
//
//	type yyLexer interface {
//		Lex(lval *yySymType) int
//		Error(e string)
//	}
//
// Optionally the argument to yyParse may implement the following interface:
//
//	type yyLexerEx interface {
//		yyLexer
//		// Hook for recording a reduction.
//		Reduced(rule, state int, lval *yySymType) (stop bool) // Client should copy *lval.
//	}
//
// Lex should return the token identifier, and place other token information in
// lval (which replaces the usual yylval). Error is equivalent to yyerror in
// the original yacc.
//
// Code inside the parser may refer to the variable yylex, which holds the
// yyLexer passed to Parse.
//
// Multiple grammars compiled into a single program should be placed in
// distinct packages. If that is impossible, the "-p prefix" flag to yacc sets
// the prefix, by default yy, that begins the names of symbols, including
// types, the parser, and the lexer, generated and referenced by yacc's
// generated code. Setting it to distinct values allows multiple grammars to be
// placed in a single package.
//
// Differences wrt go tool yacc
//
// - goyacc implements ideas from "Generating LR Syntax Error Messages from
// Examples"[1]. Use the -xe flag to pass a name of the example file. For more
// details about the example format please see [2].
//
// - The grammar report includes example token sequences leading to the
// particular state. Can help understanding conflicts.
//
// - Minor changes in parser debug output.
//
// Links
//
// Referenced from elsewhere:
//
//  [0]: http://golang.org/cmd/yacc/
//  [1]: http://people.via.ecp.fr/~stilgar/doc/compilo/parser/Generating%20LR%20Syntax%20Error%20Messages.pdf
//  [2]: http://godoc.org/github.com/cznic/y#hdr-Error_Examples
//  [3]: http://www.gnu.org/software/bison/manual/html_node/Precedence-Only.html#Precedence-Only
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"go/scanner"
	"go/token"
	"io"
	"io/ioutil"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/skycoin/cx/yacc/parser/yacc"
	"github.com/skycoin/cx/yacc/y"

	"github.com/skycoin/cx/yacc/mathutil"
	"github.com/kycoin/cx/yacc/sortutil"
	"github.com/kycoin/cx/yacc/strutil"
)

var (
	oClosures   = flag.Bool("c", false, "report state closures")
	oDlval      = flag.String("dlval", "lval", "debug value (runtime yyDebug >= 3)")
	oDlvalf     = flag.String("dlvalf", "%+v", "debug format of -dlval (runtime yyDebug >= 3)")
	oFollowSets = flag.Bool("fs", false, "emit the follow set table")
	oLA         = flag.Bool("la", false, "report all lookahead sets")
	oNoLines    = flag.Bool("l", false, "disable line directives (for compatibility ony - ignored)")
	oOut        = flag.String("o", "y.go", "parser output")
	oPool       = flag.Bool("pool", false, "uses sync.Pool to recycle parser stacks")
	oPref       = flag.String("p", "yy", "name prefix to use in generated code")
	oReducible  = flag.Bool("cr", false, "check all states are reducible")
	oReport     = flag.String("v", "y.output", "create grammar report")
	oResolved   = flag.Bool("ex", false, "explain how were conflicts resolved")
	oXErrors    = flag.String("xe", "", "generate eXtra errors from examples source file")
	oXErrorsGen = flag.String("xegen", "", "generate error from examples source file automatically from the grammar")
)

func main() {
	log.SetFlags(0)
	flag.Parse()
	var in string
	switch flag.NArg() {
	case 0:
		in = os.Stdin.Name()
	case 1:
		in = flag.Arg(0)
	default:
		log.Fatal("expected at most one non flag argument")
	}

	if err := main1(in); err != nil {
		switch x := err.(type) {
		case scanner.ErrorList:
			for _, v := range x {
				fmt.Fprintf(os.Stderr, "%v\n", v)
			}
			os.Exit(1)
		default:
			log.Fatal(err)
		}
	}
}

type symUsed struct {
	sym  *y.Symbol
	used int
}

type symsUsed []symUsed

func (s symsUsed) Len() int      { return len(s) }
func (s symsUsed) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

func (s symsUsed) Less(i, j int) bool {
	if s[i].used > s[j].used {
		return true
	}

	if s[i].used < s[j].used {
		return false
	}

	return strings.ToLower(s[i].sym.Name) < strings.ToLower(s[j].sym.Name)
}

func main1(in string) (err error) {
	var out io.Writer
	if nm := *oOut; nm != "" {
		var f *os.File
		var e error
		if f, err = os.Create(nm); err != nil {
			return err
		}

		defer func() {
			if e := f.Close(); e != nil && err == nil {
				err = e
			}
		}()
		w := bufio.NewWriter(f)
		defer func() {
			if e := w.Flush(); e != nil && err == nil {
				err = e
			}
		}()
		buf := bytes.NewBuffer(nil)
		out = buf
		defer func() {
			var dest []byte
			if dest, e = format.Source(buf.Bytes()); e != nil {
				dest = buf.Bytes()
			}

			if _, e = w.Write(dest); e != nil && err == nil {
				err = e
			}
		}()
	}

	var rep io.Writer
	if nm := *oReport; nm != "" {
		f, err := os.Create(nm)
		if err != nil {
			return err
		}

		defer func() {
			if e := f.Close(); e != nil && err == nil {
				err = e
			}
		}()
		w := bufio.NewWriter(f)
		defer func() {
			if e := w.Flush(); e != nil && err == nil {
				err = e
			}
		}()
		rep = w
	}

	var xerrors []byte
	if nm := *oXErrors; nm != "" {
		b, err := ioutil.ReadFile(nm)
		if err != nil {
			return err
		}

		xerrors = b
	}

	p, err := y.ProcessFile(token.NewFileSet(), in, &y.Options{
		//NoDefault:   *oNoDefault,
		AllowConflicts: true,
		Closures:       *oClosures,
		LA:             *oLA,
		Reducible:      *oReducible,
		Report:         rep,
		Resolved:       *oResolved,
		XErrorsName:    *oXErrors,
		XErrorsSrc:     xerrors,
	})
	if err != nil {
		return err
	}

	if fn := *oXErrorsGen; fn != "" {
		f, err := os.OpenFile(fn, os.O_RDWR|os.O_CREATE, 0666)
		if err != nil {
			return err
		}

		b := bufio.NewWriter(f)
		if err := p.SkeletonXErrors(b); err != nil {
			return err
		}

		if err := b.Flush(); err != nil {
			return err
		}

		if err := f.Close(); err != nil {
			return err
		}
	}

	msu := make(map[*y.Symbol]int, len(p.Syms)) // sym -> usage
	for nm, sym := range p.Syms {
		if nm == "" || nm == "ε" || nm == "$accept" || nm == "#" {
			continue
		}

		msu[sym] = 0
	}
	var minArg, maxArg int
	for _, state := range p.Table {
		for _, act := range state {
			msu[act.Sym]++
			k, arg := act.Kind()
			if k == 'a' {
				continue
			}

			if k == 'r' {
				arg = -arg
			}
			minArg, maxArg = mathutil.Min(minArg, arg), mathutil.Max(maxArg, arg)
		}
	}
	su := make(symsUsed, 0, len(msu))
	for sym, used := range msu {
		su = append(su, symUsed{sym, used})
	}
	sort.Sort(su)

	// ----------------------------------------------------------- Prologue
	f := strutil.IndentFormatter(out, "\t")
	f.Format("// Code generated by goyacc - DO NOT EDIT.\n\n")
	f.Format("%s", injectImport(p.Prologue))
	if *oPool {
		f.Format(`
var %[1]sPool = __sync__.Pool{New: func() interface{} { s := make([]%[1]sSymType, 200); return &s }}
`, *oPref)
	}
	f.Format(`
type %[1]sSymType %i%s%u

type %[1]sXError struct {
	state, xsym int
}
`, *oPref, p.UnionSrc)

	// ---------------------------------------------------------- Constants
	nsyms := map[string]*y.Symbol{}
	a := make([]string, 0, len(msu))
	maxTokName := 0
	for sym := range msu {
		nm := sym.Name
		if nm == "$default" || nm == "$end" || sym.IsTerminal && nm[0] != '\'' && sym.Value > 0 {
			maxTokName = mathutil.Max(maxTokName, len(nm))
			a = append(a, nm)
		}
		nsyms[nm] = sym
	}
	sort.Strings(a)
	f.Format("\nconst (%i\n")
	for _, v := range a {
		nm := v
		switch nm {
		case "error":
			nm = *oPref + "ErrCode"
		case "$default":
			nm = *oPref + "Default"
		case "$end":
			nm = *oPref + "EofCode"
		}
		f.Format("%s%s = %d\n", nm, strings.Repeat(" ", maxTokName-len(nm)+1), nsyms[v].Value)
	}
	minArg-- // eg: [-13, 42], minArg -14 maps -13 to 1 so zero cell values -> empty.
	f.Format("\n%sMaxDepth = 200\n", *oPref)
	f.Format("%sTabOfs   = %d\n", *oPref, minArg)
	f.Format("%u)")

	// ---------------------------------------------------------- Variables
	f.Format("\n\nvar (%i\n")

	f.Format("\n%sPrec = map[int]int{%i\n", *oPref)
	for i, v := range p.AssocDefs {
		for _, w := range v.Syms {
			if !w.IsTerminal {
				continue
			}

			f.Format("%s: %v,\n", w.Name, i)
		}
	}
	f.Format("}%u\n\n")

	if *oFollowSets {
		f.Format("%sFollow = [][]int{%i\n", *oPref)
		for state, action := range p.Table {
			f.Format("{")
			for _, a := range action {
				f.Format("%v, ", a.Sym.Value)
			}
			f.Format("}, // state %v\n", state)
		}
		f.Format("%u}\n\n")
	}

	// Lex translation table
	f.Format("%sXLAT = map[int]int{%i\n", *oPref)
	xlat := make(map[int]int, len(su))
	var errSym int
	for i, v := range su {
		if v.sym.Name == "error" {
			errSym = i
		}
		xlat[v.sym.Value] = i
		f.Format("%6d: %3d, // %s (%dx)\n", v.sym.Value, i, v.sym.Name, msu[v.sym])
	}
	f.Format("%u}\n")

	// Symbol names
	f.Format("\n%sSymNames = []string{%i\n", *oPref)
	for _, v := range su {
		f.Format("%q,\n", strings.TrimSpace(v.sym.Name))
	}
	f.Format("%u}\n")

	// Token literal strings
	f.Format("\n%sTokenLiteralStrings = map[int]string{%i\n", *oPref)
	for _, v := range su {
		if sym := v.sym; sym.IsTerminal {
			ls := sym.LiteralString
			ls, _ = strconv.Unquote(ls)
			ls = strings.TrimSpace(ls)
			if ls == "" {
				continue
			}

			f.Format("%d: %q,\n", sym.Value, ls)
		}
	}
	f.Format("%u}\n")

	// Reduction table
	f.Format("\n%sReductions = map[int]struct{xsym, components int}{%i\n", *oPref)
	for r, rule := range p.Rules {
		f.Format("%d: {%d, %d},\n", r, xlat[rule.Sym.Value], len(rule.Components))
	}
	f.Format("%u}\n")

	// XError table
	f.Format("\n%[1]sXErrors = map[%[1]sXError]string{%i\n", *oPref)
	for _, xerr := range p.XErrors {
		state := xerr.Stack[len(xerr.Stack)-1]
		xsym := -1
		if xerr.Lookahead != nil {
			xsym = xlat[xerr.Lookahead.Value]
		}
		f.Format("%[1]sXError{%d, %d}: \"%s\",\n", *oPref, state, xsym, xerr.Msg)
	}
	f.Format("%u}\n\n")

	// Parse table
	tbits := 32
	switch n := mathutil.BitLen(maxArg - minArg + 1); {
	case n < 8:
		tbits = 8
	case n < 16:
		tbits = 16
	}
	f.Format("%sParseTab = [%d][]uint%d{%i\n", *oPref, len(p.Table), tbits)
	nCells := 0
	var tabRow sortutil.Uint64Slice
	for si, state := range p.Table {
		tabRow = tabRow[:0]
		max := 0
		for _, act := range state {
			sym := act.Sym
			xsym, ok := xlat[sym.Value]
			if !ok {
				panic("internal error 001")
			}

			max = mathutil.Max(max, xsym)
			kind, arg := act.Kind()
			switch kind {
			case 'a':
				arg = 0
			case 'r':
				arg *= -1
			}
			tabRow = append(tabRow, uint64(xsym)<<32|uint64(arg-minArg))
		}
		nCells += max
		tabRow.Sort()
		col := -1
		if si%5 == 0 {
			f.Format("// %d\n", si)
		}
		f.Format("{")
		for i, v := range tabRow {
			xsym := int(uint32(v >> 32))
			arg := int(uint32(v))
			if col+1 != xsym {
				f.Format("%d: ", xsym)
			}
			switch {
			case i == len(tabRow)-1:
				f.Format("%d", arg)
			default:
				f.Format("%d, ", arg)
			}
			col = xsym
		}
		f.Format("},\n")
	}
	f.Format("%u}\n")
	fmt.Fprintf(os.Stderr, "Parse table entries: %d of %d, x %d bits == %d bytes\n", nCells, len(p.Table)*len(msu), tbits, nCells*tbits/8)
	if n := p.ConflictsSR; n != 0 {
		fmt.Fprintf(os.Stderr, "conflicts: %d shift/reduce\n", n)
	}
	if n := p.ConflictsRR; n != 0 {
		fmt.Fprintf(os.Stderr, "conflicts: %d reduce/reduce\n", n)
	}

	makeYYS := fmt.Sprintf("yyS := make([]%[1]sSymType, 200)\n", *oPref)
	if *oPool {
		makeYYS = fmt.Sprintf(`p := %[1]sPool.Get().(*[]%[1]sSymType)
yyS := *p

defer func() {
	var v %[1]sSymType
	for i := range yyS {
		yyS[i] = v
	}
	%[1]sPool.Put(p)
}()
`, *oPref)
	}

	f.Format(`%u)

var %[1]sDebug = 0

type %[1]sLexer interface {
	Lex(lval *%[1]sSymType) int
	Error(s string)
}

type %[1]sLexerEx interface {
	%[1]sLexer
	Reduced(rule, state int, lval *%[1]sSymType) bool
}

func %[1]sSymName(c int) (s string) {
	x, ok := %[1]sXLAT[c]
	if ok {
		return %[1]sSymNames[x]
	}

	if c < 0x7f {
		return __yyfmt__.Sprintf("%%q", c)
	}

	return __yyfmt__.Sprintf("%%d", c)
}

func %[1]slex1(yylex %[1]sLexer, lval *%[1]sSymType) (n int) {
	n = yylex.Lex(lval)
	if n <= 0 {
		n = %[1]sEofCode
	}
	if %[1]sDebug >= 3 {
		__yyfmt__.Printf("\nlex %%s(%%#x %%d), %[4]s: %[3]s\n", %[1]sSymName(n), n, n, %[4]s)
	}
	return n
}
	
func %[1]sParse(yylex %[1]sLexer) int {
	const yyError = %[2]d

	yyEx, _ := yylex.(%[1]sLexerEx)
	var yyn int
	var yylval %[1]sSymType
	var yyVAL %[1]sSymType
	%[5]s

	Nerrs := 0   /* number of errors */
	Errflag := 0 /* error recovery flag */
	yyerrok := func() { 
		if %[1]sDebug >= 2 {
			__yyfmt__.Printf("yyerrok()\n")
		}
		Errflag = 0
	}
	_ = yyerrok
	yystate := 0
	yychar := -1
	var yyxchar int
	var yyshift int
	yyp := -1
	goto yystack

ret0:
	return 0

ret1:
	return 1

yystack:
	/* put a state and value onto the stack */
	yyp++
	if yyp >= len(yyS) {
		nyys := make([]%[1]sSymType, len(yyS)*2)
		copy(nyys, yyS)
		yyS = nyys
	}
	yyS[yyp] = yyVAL
	yyS[yyp].yys = yystate

yynewstate:
	if yychar < 0 {
		yylval.yys = yystate
		yychar = %[1]slex1(yylex, &yylval)
		var ok bool
		if yyxchar, ok = %[1]sXLAT[yychar]; !ok {
			yyxchar = len(%[1]sSymNames) // > tab width
		}
	}
	if %[1]sDebug >= 4 {
		var a []int
		for _, v := range yyS[:yyp+1] {
			a = append(a, v.yys)
		}
		__yyfmt__.Printf("state stack %%v\n", a)
	}
	row := %[1]sParseTab[yystate]
	yyn = 0
	if yyxchar < len(row) {
		if yyn = int(row[yyxchar]); yyn != 0 {
			yyn += %[1]sTabOfs
		}
	}
	switch {
	case yyn > 0: // shift
		yychar = -1
		yyVAL = yylval
		yystate = yyn
		yyshift = yyn
		if %[1]sDebug >= 2 {
			__yyfmt__.Printf("shift, and goto state %%d\n", yystate)
		}
		if Errflag > 0 {
			Errflag--
		}
		goto yystack
	case yyn < 0: // reduce
	case yystate == 1: // accept
		if %[1]sDebug >= 2 {
			__yyfmt__.Println("accept")
		}
		goto ret0
	}

	if yyn == 0 {
		/* error ... attempt to resume parsing */
		switch Errflag {
		case 0: /* brand new error */
			if %[1]sDebug >= 1 {
				__yyfmt__.Printf("no action for %%s in state %%d\n", %[1]sSymName(yychar), yystate)
			}
			msg, ok := %[1]sXErrors[%[1]sXError{yystate, yyxchar}]
			if !ok {
				msg, ok = %[1]sXErrors[%[1]sXError{yystate, -1}]
			}
			if !ok && yyshift != 0 {
				msg, ok = %[1]sXErrors[%[1]sXError{yyshift, yyxchar}]
			}
			if !ok {
				msg, ok = %[1]sXErrors[%[1]sXError{yyshift, -1}]
			}
			if yychar > 0 {
				ls := %[1]sTokenLiteralStrings[yychar]
				if ls == "" {
					ls = %[1]sSymName(yychar)
				}
				if ls != "" {
					switch {
					case msg == "":
						msg = __yyfmt__.Sprintf("unexpected %%s", ls)
					default:
						msg = __yyfmt__.Sprintf("unexpected %%s, %%s", ls, msg)
					}
				}
			}
			if msg == "" {
				msg = "syntax error"
			}
			yylex.Error(msg)
			Nerrs++
			fallthrough

		case 1, 2: /* incompletely recovered error ... try again */
			Errflag = 3

			/* find a state where "error" is a legal shift action */
			for yyp >= 0 {
				row := %[1]sParseTab[yyS[yyp].yys]
				if yyError < len(row) {
					yyn = int(row[yyError])+%[1]sTabOfs
					if yyn > 0 { // hit
						if %[1]sDebug >= 2 {
							__yyfmt__.Printf("error recovery found error shift in state %%d\n", yyS[yyp].yys)
						}
						yystate = yyn /* simulate a shift of "error" */
						goto yystack
					}
				}

				/* the current p has no shift on "error", pop stack */
				if %[1]sDebug >= 2 {
					__yyfmt__.Printf("error recovery pops state %%d\n", yyS[yyp].yys)
				}
				yyp--
			}
			/* there is no state on the stack with an error shift ... abort */
			if %[1]sDebug >= 2 {
				__yyfmt__.Printf("error recovery failed\n")
			}
			goto ret1

		case 3: /* no shift yet; clobber input char */
			if %[1]sDebug >= 2 {
				__yyfmt__.Printf("error recovery discards %%s\n", %[1]sSymName(yychar))
			}
			if yychar == %[1]sEofCode {
				goto ret1
			}

			yychar = -1
			goto yynewstate /* try again in the same state */
		}
	}

	r := -yyn
	x0 := %[1]sReductions[r]
	x, n := x0.xsym, x0.components
	yypt := yyp
	_ = yypt // guard against "declared and not used"

	yyp -= n
	if yyp+1 >= len(yyS) {
		nyys := make([]%[1]sSymType, len(yyS)*2)
		copy(nyys, yyS)
		yyS = nyys
	}
	yyVAL = yyS[yyp+1]

	/* consult goto table to find next state */
	exState := yystate
	yystate = int(%[1]sParseTab[yyS[yyp].yys][x])+%[1]sTabOfs
	/* reduction by production r */
	if %[1]sDebug >= 2 {
		__yyfmt__.Printf("reduce using rule %%v (%%s), and goto state %%d\n", r, %[1]sSymNames[x], yystate)
	}

	switch r {%i
`,
		*oPref, errSym, *oDlvalf, *oDlval, makeYYS)
	for r, rule := range p.Rules {
		if rule.Action == nil {
			continue
		}

		action := rule.Action.Values
		if len(action) == 0 {
			continue
		}

		if len(action) == 1 {
			part := action[0]
			if part.Type == parser.ActionValueGo {
				src := part.Src
				src = src[1 : len(src)-1] // Remove lead '{' and trail '}'
				if strings.TrimSpace(src) == "" {
					continue
				}
			}
		}

		components := rule.Components
		typ := rule.Sym.Type
		max := len(components)
		if p := rule.Parent; p != nil {
			max = rule.MaxParentDlr
			components = p.Components
		}
		f.Format("case %d: ", r)
		for _, part := range action {
			num := part.Num
			switch part.Type {
			case parser.ActionValueGo:
				f.Format("%s", part.Src)
			case parser.ActionValueDlrDlr:
				f.Format("yyVAL.%s", typ)
				if typ == "" {
					panic("internal error 002")
				}
			case parser.ActionValueDlrNum:
				typ := p.Syms[components[num-1]].Type
				if typ == "" {
					panic("internal error 003")
				}
				f.Format("yyS[yypt-%d].%s", max-num, typ)
			case parser.ActionValueDlrTagDlr:
				f.Format("yyVAL.%s", part.Tag)
			case parser.ActionValueDlrTagNum:
				f.Format("yyS[yypt-%d].%s", max-num, part.Tag)
			}
		}
		f.Format("\n")
	}
	f.Format(`%u
	}

	if yyEx != nil && yyEx.Reduced(r, exState, &yyVAL) {
		return -1
	}
	goto yystack /* stack new state and value */
}

%[2]s
`, *oPref, p.Tail)
	_ = oNoLines //TODO Ignored for now
	return nil
}

func injectImport(src string) string {
	const inj0 = `

import __yyfmt__ "fmt"
`
	inj := inj0
	if *oPool {
		inj += `import __sync__ "sync"
`
	}
	fset := token.NewFileSet()
	file := fset.AddFile("", -1, len(src))
	var s scanner.Scanner
	s.Init(
		file,
		[]byte(src),
		nil,
		scanner.ScanComments,
	)
	for {
		switch _, tok, _ := s.Scan(); tok {
		case token.EOF:
			return inj + src
		case token.PACKAGE:
			s.Scan() // ident
			pos, _, _ := s.Scan()
			ofs := file.Offset(pos)
			return src[:ofs] + inj + src[ofs:]
		}
	}
}
