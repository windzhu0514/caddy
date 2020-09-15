// Copyright 2015 Light Code Labs, LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package caddyfile

import (
	"bufio"
	"bytes"
	"io"
	"unicode"
)

type (
	// lexer 从reader中逐个标识符的获取值。每个token都是一个单词，每个token由空白字符分隔。
	// 单词中如果有空白字符，需要使用双引号括起来。

	// lexer is a utility which can get values, token by
	// token, from a Reader. A token is a word, and tokens
	// are separated by whitespace. A word can be enclosed
	// in quotes if it contains whitespace.
	lexer struct {
		reader       *bufio.Reader
		token        Token
		line         int
		skippedLines int
	}

	// Token represents a single parsable unit.
	Token struct {
		File string
		Line int
		Text string
	}
)

// load prepares the lexer to scan an input for tokens.
// It discards any leading byte order mark.
func (l *lexer) load(input io.Reader) error {
	l.reader = bufio.NewReader(input)
	l.line = 1

	// discard byte order mark, if present
	firstCh, _, err := l.reader.ReadRune()
	if err != nil {
		return err
	}
	if firstCh != 0xFEFF {
		err := l.reader.UnreadRune()
		if err != nil {
			return err
		}
	}

	return nil
}

// next 读取下一个token到lexer中
// token由空白字符分隔，双引号包括token中可以由空白字符。
// 双引号包括的字符串中，引号可能被转义。其他字符不会被转移。
// 如果读取到#，#号后面的字符串被舍弃。
// 如果token读取成功，返回true，否则返回false

// next loads the next token into the lexer.
// A token is delimited by whitespace, unless
// the token starts with a quotes character (")
// in which case the token goes until the closing
// quotes (the enclosing quotes are not included).
// Inside quoted strings, quotes may be escaped
// with a preceding \ character. No other chars
// may be escaped. The rest of the line is skipped
// if a "#" character is read in. Returns true if
// a token was loaded; false otherwise.
func (l *lexer) next() bool {
	var val []rune
	var comment, quoted, btQuoted, escaped bool

	makeToken := func() bool {
		l.token.Text = string(val)
		return true
	}

	for {
		ch, _, err := l.reader.ReadRune()
		if err != nil {
			if len(val) > 0 {
				return makeToken()
			}
			if err == io.EOF {
				return false
			}
			panic(err)
		}

		if !escaped && !btQuoted && ch == '\\' {
			escaped = true
			continue
		}

		if quoted || btQuoted {
			if quoted && escaped {
				// all is literal in quoted area,
				// so only escape quotes
				if ch != '"' {
					val = append(val, '\\')
				}
				escaped = false
			} else {
				if quoted && ch == '"' {
					return makeToken()
				}
				if btQuoted && ch == '`' {
					return makeToken()
				}
			}
			if ch == '\n' {
				l.line += 1 + l.skippedLines
				l.skippedLines = 0
			}
			val = append(val, ch)
			continue
		}

		if unicode.IsSpace(ch) {
			if ch == '\r' {
				continue
			}
			if ch == '\n' {
				if escaped {
					l.skippedLines++
					escaped = false
				} else {
					l.line += 1 + l.skippedLines
					l.skippedLines = 0
				}
				comment = false
			}
			if len(val) > 0 {
				return makeToken()
			}
			continue
		}

		if ch == '#' && len(val) == 0 {
			comment = true
		}
		if comment {
			continue
		}

		if len(val) == 0 {
			l.token = Token{Line: l.line}
			if ch == '"' {
				quoted = true
				continue
			}
			if ch == '`' {
				btQuoted = true
				continue
			}
		}

		if escaped {
			val = append(val, '\\')
			escaped = false
		}

		val = append(val, ch)
	}
}

// Tokenize takes bytes as input and lexes it into
// a list of tokens that can be parsed as a Caddyfile.
// Also takes a filename to fill the token's File as
// the source of the tokens, which is important to
// determine relative paths for `import` directives.
func Tokenize(input []byte, filename string) ([]Token, error) {
	l := lexer{}
	if err := l.load(bytes.NewReader(input)); err != nil {
		return nil, err
	}
	var tokens []Token
	for l.next() {
		l.token.File = filename
		tokens = append(tokens, l.token)
	}
	return tokens, nil
}
