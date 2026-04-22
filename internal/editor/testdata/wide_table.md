# Wide table repro

Intro paragraph with a CJK word 한 and combining-mark cluster café.

┌──────────────────────────────────────────────────────────────────────────────┐
│ Banner line above the table — intentionally wider than any 80-col viewport. │
└──────────────────────────────────────────────────────────────────────────────┘

| Col A | Col B | Col C |
|-------|-------|-------|
| 1     | 日本語 | foo   |
| 2     | 한    | bar   |
| 3     | café  | baz   |

Another horizontal rule:

├──────────────────────────────────────────────────────────────────────────────┤

Some prose between the table and the fenced block so the row layout varies.

```
┏━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓
┃ fenced code banner ┃ payload ┃ note                                         ┃
┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛
```

Trailing content to push the file past 24 lines:

- item one
- item two
- item three
- item four
- item five
