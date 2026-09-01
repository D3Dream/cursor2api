#!/usr/bin/env python3
"""把 cursor-agent bundle (index.js) 拆成 webpack 模块，输出 modules.js。

bundle 结构: {"./path/mod.js"(e,t,n){ ... }, ...}
用带状态的字符扫描器做平衡括号提取，处理字符串/模板串/正则/注释。
输出: build/modules.js —— `module.exports = { "id": function(e,t,n){...}, ... }`
"""
import json
import re
import sys
import os

def tokenize_modules(src: str, start: int):
    """从 start 位置（模块 map 的 '{'）顺序解析所有模块。"""
    i = start
    n = len(src)
    assert src[i] == '{'
    i += 1
    modules = {}

    def skip_ws(j):
        while j < n:
            c = src[j]
            if c in ' \t\r\n':
                j += 1
            elif src.startswith('//', j):
                k = src.find('\n', j)
                j = n if k < 0 else k + 1
            elif src.startswith('/*', j):
                k = src.find('*/', j + 2)
                if k < 0:
                    raise ValueError('unterminated comment')
                j = k + 2
            else:
                break
        return j

    # 判断 / 是正则还是除法：看前一个有效 token
    REGEX_KEYWORDS = {
        'return', 'typeof', 'case', 'in', 'of', 'new', 'delete', 'void',
        'throw', 'else', 'do', 'instanceof', 'yield', 'await', 'function',
    }

    def is_regex_ctx(j):
        k = j - 1
        while k >= 0 and src[k] in ' \t\r\n':
            k -= 1
        if k < 0:
            return True
        c = src[k]
        if c in '(,=:[!&|?{};+-*%^~<>':
            return True
        if c.isalnum() or c in '_$':
            # 提取前面的标识符，判断是否关键字
            e = k + 1
            while k >= 0 and (src[k].isalnum() or src[k] in '_$'):
                k -= 1
            return src[k + 1:e] in REGEX_KEYWORDS
        return False

    i = skip_ws(i)
    while i < n and src[i] != '}':
        # 模块 key: "..." / '...' / 裸标识符（node builtin shim）
        if src[i] in '"\'':
            q = src[i]
            j = i + 1
            # 越界保护：损坏的 bundle（未闭合引号）不能 IndexError，
            # 报出可读错误比 traceback 有用
            while j < n and src[j] != q:
                if src[j] == '\\':
                    j += 1
                j += 1
            if j >= n:
                raise ValueError(f"unterminated string at offset {i} (corrupt bundle?)")
            key = src[i + 1:j]
            i = skip_ws(j + 1)
        else:
            m2 = re.compile(r'[\w$]+').match(src, i)
            if not m2:
                raise ValueError(f'expected module key at {i}: {src[i:i+60]!r}')
            key = m2.group(0)
            i = skip_ws(m2.end())

        # 函数头: (e,t,n){ 或 (e,t,n)=>{ 或无参变体
        m = re.compile(r'\(([^)]*)\)\s*(?:=>\s*)?\{').match(src, i)
        if not m:
            raise ValueError(f'expected function for {key} at {i}: {src[i:i+60]!r}')
        body_start = m.end() - 1  # 指向 '{'
        header = src[i:body_start]

        # 平衡扫描函数体
        depth = 0
        k = body_start
        # 状态栈: 'code' | 'tmpl'；${ 进入的 code 状态记录其深度以便弹回 tmpl
        stack = ['code']
        code_depths = []
        while k < n:
            c = src[k]
            st = stack[-1]
            if st == 'code':
                if c == '{':
                    depth += 1
                elif c == '}':
                    depth -= 1
                    if code_depths and depth == code_depths[-1] - 1:
                        # 关闭模板串的 ${，回到 tmpl
                        stack.pop()
                        code_depths.pop()
                    elif depth == 0:
                        k += 1
                        break
                elif c in '"\'':
                    q2 = c
                    k += 1
                    while k < n and src[k] != q2:
                        if src[k] == '\\':
                            k += 1
                        k += 1
                elif c == '`':
                    stack.append('tmpl')
                elif c == '/' and src.startswith('//', k):
                    e2 = src.find('\n', k)
                    k = n if e2 < 0 else e2
                elif c == '/' and src.startswith('/*', k):
                    e2 = src.find('*/', k + 2)
                    if e2 < 0:
                        raise ValueError('unterminated comment in body')
                    k = e2 + 1
                elif c == '/' and is_regex_ctx(k):
                    # 正则字面量
                    k += 1
                    in_class = False
                    while k < n:
                        cc = src[k]
                        if cc == '\\':
                            k += 1
                        elif cc == '[':
                            in_class = True
                        elif cc == ']':
                            in_class = False
                        elif cc == '/' and not in_class:
                            break
                        k += 1
                k += 1
            else:  # tmpl
                if c == '\\':
                    k += 2
                    continue
                if c == '`':
                    stack.pop()
                elif src.startswith('${', k):
                    stack.append('code')
                    depth += 1
                    code_depths.append(depth)
                    k += 1
                k += 1
        else:
            raise ValueError(f'unterminated body for {key}')

        modules[key] = header + src[body_start:k]
        i = skip_ws(k)
        if i < n and src[i] == ',':
            i = skip_ws(i + 1)

    return modules


def main():
    bundle = sys.argv[1]
    out = sys.argv[2]
    src = open(bundle, encoding='utf-8').read()

    # 找模块 map 起点：__webpack_modules__={
    anchor = src.find('__webpack_modules__={')
    if anchor < 0:
        print('module map not found', file=sys.stderr)
        sys.exit(1)
    start = src.index('{', anchor)
    modules = tokenize_modules(src, start)
    print(f'extracted {len(modules)} modules')

    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(out, 'w', encoding='utf-8') as f:
        f.write('// generated by extract_modules.py\nmodule.exports = {\n')
        for key, fn in modules.items():
            f.write(json.dumps(key) + ': function' + fn + ',\n')
        f.write('};\n')
    print(f'wrote {out}')


if __name__ == '__main__':
    main()
