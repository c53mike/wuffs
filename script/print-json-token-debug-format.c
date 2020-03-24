// Copyright 2020 The Wuffs Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// ----------------

// print-json-token-debug-format.c parses JSON from stdin and prints the
// resulting token stream, eliding any non-essential (e.g. whitespace) tokens.
//
// The output format is only for debugging or regression testing, and certainly
// not for long term storage. It isn't guaranteed to be stable between versions
// of this program and of the Wuffs standard library.
//
// It prints 16 bytes (128 bits) per token, containing big-endian numbers:
//
// +---+---+---+---+---+---+---+---+---+---+---+---+---+---+---+---+
// |      POS      |  LEN  | LP| LN|     MAJOR     |     MINOR     |
// |               |       |   |   |               |VBC|    VBD    |
// +---+---+---+---+---+---+---+---+---+---+---+---+---+---+---+---+
//
//  - POS   (4 bytes) is the position: the sum of all previous tokens' lengths,
//                    including elided tokens.
//  - LEN   (2 bytes) is the length.
//  - LP    (1 bytes) is the link_prev bit.
//  - LN    (1 bytes) is the link_next bit
//  - MAJOR (4 bytes) is the value_major.
//
// The final 4 bytes are either value_minor (when the value_major is non-zero)
// or 1 + 3 bytes for value_base_category and value_base_detail (otherwise).
//
// Together with the hexadecimal WUFFS_BASE__TOKEN__ETC constants defined in
// token-public.h, this format is somewhat human-readable when piped through a
// hex-dump program (such as /usr/bin/hd), printing one token per line.
// Alternatively, pass the -h (--human-readable) flag to this program.
//
// Pass -a (--all-tokens) to print all tokens, including whitespace.
//
// If the input or output is larger than the program's buffers (64 MiB and
// 131072 tokens by default), there may be multiple valid tokenizations of any
// given input. For example, if a source string "abcde" straddles an I/O
// boundary, it may be tokenized as single (no-link) 5-length string or as a
// 3-length link_next'ed string followed by a 2-length link_prev'ed string.
//
// A Wuffs token stream, in general, can support inputs more than 0xFFFF_FFFF
// bytes long, but this program can not, as it tracks the tokens' cumulative
// position as a uint32.

#include <inttypes.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

// Wuffs ships as a "single file C library" or "header file library" as per
// https://github.com/nothings/stb/blob/master/docs/stb_howto.txt
//
// To use that single file as a "foo.c"-like implementation, instead of a
// "foo.h"-like header, #define WUFFS_IMPLEMENTATION before #include'ing or
// compiling it.
#define WUFFS_IMPLEMENTATION

// Defining the WUFFS_CONFIG__MODULE* macros are optional, but it lets users of
// release/c/etc.c whitelist which parts of Wuffs to build. That file contains
// the entire Wuffs standard library, implementing a variety of codecs and file
// formats. Without this macro definition, an optimizing compiler or linker may
// very well discard Wuffs code for unused codecs, but listing the Wuffs
// modules we use makes that process explicit. Preprocessing means that such
// code simply isn't compiled.
#define WUFFS_CONFIG__MODULES
#define WUFFS_CONFIG__MODULE__BASE
#define WUFFS_CONFIG__MODULE__JSON

// If building this program in an environment that doesn't easily accommodate
// relative includes, you can use the script/inline-c-relative-includes.go
// program to generate a stand-alone C++ file.
#include "../release/c/wuffs-unsupported-snapshot.c"

#ifndef SRC_BUFFER_ARRAY_SIZE
#define SRC_BUFFER_ARRAY_SIZE (64 * 1024 * 1024)
#endif
#ifndef TOKEN_BUFFER_ARRAY_SIZE
#define TOKEN_BUFFER_ARRAY_SIZE (128 * 1024)
#endif

uint8_t src_buffer_array[SRC_BUFFER_ARRAY_SIZE];
wuffs_base__token tok_buffer_array[TOKEN_BUFFER_ARRAY_SIZE];

wuffs_base__io_buffer src;
wuffs_base__token_buffer tok;

wuffs_json__decoder dec;
wuffs_base__status dec_status;

#define TRY(error_msg)         \
  do {                         \
    const char* z = error_msg; \
    if (z) {                   \
      return z;                \
    }                          \
  } while (false)

// ignore_return_value suppresses errors from -Wall -Werror.
static void  //
ignore_return_value(int ignored) {}

const char*  //
read_src() {
  if (src.meta.closed) {
    return "main: internal error: read requested on a closed source";
  }
  wuffs_base__io_buffer__compact(&src);
  if (src.meta.wi >= src.data.len) {
    return "main: src buffer is full";
  }
  size_t n = fread(src.data.ptr + src.meta.wi, sizeof(uint8_t),
                   src.data.len - src.meta.wi, stdin);
  src.meta.wi += n;
  src.meta.closed = feof(stdin);
  if ((n == 0) && !src.meta.closed) {
    return "main: read error";
  }
  return NULL;
}

// ----

struct {
  int remaining_argc;
  char** remaining_argv;

  bool all_tokens;
  bool human_readable;
  bool quirks;
} flags = {0};

const char*  //
parse_flags(int argc, char** argv) {
  int c = (argc > 0) ? 1 : 0;  // Skip argv[0], the program name.
  for (; c < argc; c++) {
    char* arg = argv[c];
    if (*arg++ != '-') {
      break;
    }

    // A double-dash "--foo" is equivalent to a single-dash "-foo". As special
    // cases, a bare "-" is not a flag (some programs may interpret it as
    // stdin) and a bare "--" means to stop parsing flags.
    if (*arg == '\x00') {
      break;
    } else if (*arg == '-') {
      arg++;
      if (*arg == '\x00') {
        c++;
        break;
      }
    }

    if (!strcmp(arg, "a") || !strcmp(arg, "all-tokens")) {
      flags.all_tokens = true;
      continue;
    }
    if (!strcmp(arg, "h") || !strcmp(arg, "human-readable")) {
      flags.human_readable = true;
      continue;
    }
    if (!strcmp(arg, "q") || !strcmp(arg, "quirks")) {
      flags.quirks = true;
      continue;
    }

    return "main: unrecognized flag argument";
  }

  flags.remaining_argc = argc - c;
  flags.remaining_argv = argv + c;
  return NULL;
}

const char* vbc_names[8] = {
    "0:Filler..........",  //
    "1:Structure.......",  //
    "2:String..........",  //
    "3:UnicodeCodePoint",  //
    "4:Literal.........",  //
    "5:Number..........",  //
    "6:Reserved........",  //
    "7:Reserved........",  //
};

const int base38_decode[38] = {
    ' ', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', '?',       //
    'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', 'l', 'm',  //
    'n', 'o', 'p', 'q', 'r', 's', 't', 'u', 'v', 'w', 'x', 'y', 'z',  //
};

const char*  //
main1(int argc, char** argv) {
  TRY(parse_flags(argc, argv));
  if (flags.remaining_argc > 0) {
    return "main: bad argument: use \"program < input\", not \"program input\"";
  }

  src = wuffs_base__make_io_buffer(
      wuffs_base__make_slice_u8(src_buffer_array, SRC_BUFFER_ARRAY_SIZE),
      wuffs_base__empty_io_buffer_meta());

  tok = wuffs_base__make_token_buffer(
      wuffs_base__make_slice_token(tok_buffer_array, TOKEN_BUFFER_ARRAY_SIZE),
      wuffs_base__empty_token_buffer_meta());

  wuffs_base__status init_status = wuffs_json__decoder__initialize(
      &dec, sizeof__wuffs_json__decoder(), WUFFS_VERSION, 0);
  if (!wuffs_base__status__is_ok(&init_status)) {
    return wuffs_base__status__message(&init_status);
  }

  if (flags.quirks) {
    uint32_t quirks[] = {
        WUFFS_JSON__QUIRK_ALLOW_BACKSLASH_A,
        WUFFS_JSON__QUIRK_ALLOW_BACKSLASH_CAPITAL_U,
        WUFFS_JSON__QUIRK_ALLOW_BACKSLASH_E,
        WUFFS_JSON__QUIRK_ALLOW_BACKSLASH_QUESTION_MARK,
        WUFFS_JSON__QUIRK_ALLOW_BACKSLASH_SINGLE_QUOTE,
        WUFFS_JSON__QUIRK_ALLOW_BACKSLASH_V,
        WUFFS_JSON__QUIRK_ALLOW_BACKSLASH_X,
        WUFFS_JSON__QUIRK_ALLOW_BACKSLASH_ZERO,
        WUFFS_JSON__QUIRK_ALLOW_COMMENT_BLOCK,
        WUFFS_JSON__QUIRK_ALLOW_COMMENT_LINE,
        WUFFS_JSON__QUIRK_ALLOW_EXTRA_COMMA,
        WUFFS_JSON__QUIRK_ALLOW_INF_NAN_NUMBERS,
        WUFFS_JSON__QUIRK_ALLOW_LEADING_ASCII_RECORD_SEPARATOR,
        WUFFS_JSON__QUIRK_ALLOW_LEADING_UNICODE_BYTE_ORDER_MARK,
        WUFFS_JSON__QUIRK_ALLOW_TRAILING_NEW_LINE,
        WUFFS_JSON__QUIRK_REPLACE_INVALID_UNICODE,
        0,
    };
    uint32_t i;
    for (i = 0; quirks[i]; i++) {
      wuffs_json__decoder__set_quirk_enabled(&dec, quirks[i], true);
    }
  }

  uint64_t pos = 0;
  while (true) {
    wuffs_base__status status =
        wuffs_json__decoder__decode_tokens(&dec, &tok, &src);

    while (tok.meta.ri < tok.meta.wi) {
      wuffs_base__token* t = &tok.data.ptr[tok.meta.ri++];
      uint16_t len = wuffs_base__token__length(t);

      if (flags.all_tokens || (wuffs_base__token__value(t) != 0)) {
        uint8_t lp = wuffs_base__token__link_prev(t) ? 1 : 0;
        uint8_t ln = wuffs_base__token__link_next(t) ? 1 : 0;
        uint32_t vmajor = wuffs_base__token__value_major(t);
        uint32_t vminor = wuffs_base__token__value_minor(t);
        uint8_t vbc = wuffs_base__token__value_base_category(t);
        uint32_t vbd = wuffs_base__token__value_base_detail(t);

        if (flags.human_readable) {
          printf("pos=0x%08" PRIX32 "  len=0x%04" PRIX16 "  link=0b%d%d  ",
                 (uint32_t)(pos), len, (int)(lp), (int)(ln));

          if (vmajor) {
            uint32_t m = vmajor;
            uint32_t m0 = m / (38 * 38 * 38);
            m -= m0 * (38 * 38 * 38);
            uint32_t m1 = m / (38 * 38);
            m -= m1 * (38 * 38);
            uint32_t m2 = m / (38);
            m -= m2 * (38);
            uint32_t m3 = m;

            printf("vmajor=0x%06" PRIX32 ":%c%c%c%c  vminor=0x%06" PRIX32 "\n",
                   vmajor, base38_decode[m0], base38_decode[m1],
                   base38_decode[m2], base38_decode[m3], vminor);
          } else {
            printf("vbc=%s.  vbd=0x%06" PRIX32 "\n", vbc_names[vbc & 7], vbd);
          }

        } else {
          uint8_t buf[16];
          wuffs_base__store_u32be__no_bounds_check(&buf[0x0], (uint32_t)(pos));
          wuffs_base__store_u16be__no_bounds_check(&buf[0x4], len);
          wuffs_base__store_u8__no_bounds_check(&buf[0x0006], lp);
          wuffs_base__store_u8__no_bounds_check(&buf[0x0007], ln);
          wuffs_base__store_u32be__no_bounds_check(&buf[0x8], vmajor);
          if (vmajor) {
            wuffs_base__store_u32be__no_bounds_check(&buf[0xC], vminor);
          } else {
            wuffs_base__store_u8__no_bounds_check(&buf[0x000C], vbc);
            wuffs_base__store_u24be__no_bounds_check(&buf[0xD], vbd);
          }
          const int stdout_fd = 1;
          ignore_return_value(write(stdout_fd, &buf[0], 16));
        }
      }

      pos += len;
      if (pos > 0xFFFFFFFF) {
        return "main: input is too long";
      }
    }

    if (status.repr == NULL) {
      return NULL;
    } else if (status.repr == wuffs_base__suspension__short_read) {
      TRY(read_src());
    } else if (status.repr == wuffs_base__suspension__short_write) {
      wuffs_base__token_buffer__compact(&tok);
    } else {
      return wuffs_base__status__message(&status);
    }
  }
}

// ----

int  //
compute_exit_code(const char* status_msg) {
  if (!status_msg) {
    return 0;
  }
  size_t n = strnlen(status_msg, 2047);
  if (n >= 2047) {
    status_msg = "main: internal error: error message is too long";
    n = strnlen(status_msg, 2047);
  }
  fprintf(stderr, "%s\n", status_msg);
  // Return an exit code of 1 for regular (forseen) errors, e.g. badly
  // formatted or unsupported input.
  //
  // Return an exit code of 2 for internal (exceptional) errors, e.g. defensive
  // run-time checks found that an internal invariant did not hold.
  //
  // Automated testing, including badly formatted inputs, can therefore
  // discriminate between expected failure (exit code 1) and unexpected failure
  // (other non-zero exit codes). Specifically, exit code 2 for internal
  // invariant violation, exit code 139 (which is 128 + SIGSEGV on x86_64
  // linux) for a segmentation fault (e.g. null pointer dereference).
  return strstr(status_msg, "internal error:") ? 2 : 1;
}

int  //
main(int argc, char** argv) {
  const char* z = main1(argc, argv);
  int exit_code = compute_exit_code(z);
  return exit_code;
}
