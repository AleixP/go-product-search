# Third-party notices

The agent kit in this repository (`.claude/skills/agent-kit-*` and `.claude/references/`) includes copies of third-party material distributed under the MIT License. The notices below are kept as the licenses require. Every other file of the kit (the agents, `CLAUDE.md`, `agent-kit-effective-go`, `agent-kit-go-performance`, `agent-kit-go-performance-testing` and `agent-kit-go-testing-patterns`) was written for this repository and is not covered by these notices.

## Skills and reference from addyosmani/agent-skills

Source: https://github.com/addyosmani/agent-skills (`skills/<name>/SKILL.md` and `references/definition-of-done.md`)

Included as `agent-kit-<name>`: api-and-interface-design, code-review-and-quality, code-simplification, context-engineering, debugging-and-error-recovery, incremental-implementation, performance-optimization, planning-and-task-breakdown, security-and-hardening, source-driven-development, spec-driven-development, test-driven-development.

Also included: `.claude/references/definition-of-done.md`.

Changes: the `name:` field of each skill and the references between the skills were prefixed with `agent-kit-` (for example `test-driven-development` became `agent-kit-test-driven-development`). In `definition-of-done.md` the names of the kit skills were prefixed in the same way and a short note was added after the introduction about three skills that are not part of the kit. The text is otherwise unmodified. The other files that these skills mention under `references/` are not included.

```
MIT License

Copyright (c) 2025 Addy Osmani

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## Skill from DietrichGebert/ponytail

Source: https://github.com/DietrichGebert/ponytail (`skills/ponytail/SKILL.md`)

Included as `agent-kit-ponytail`.

Changes: only the `name:` field was changed to `agent-kit-ponytail`. The text is otherwise unmodified.

```
MIT License

Copyright (c) 2026 DietrichGebert

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
