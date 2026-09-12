# 文件编码与 Web 编辑器学习清单

这份清单用于理解和 review PlainMote 的文件上传、编码检测、在线编辑与保存链路。目标不是自己实现
字符集检测算法，而是能够判断一次改动是否会误解码、损坏原始字节，或者让浏览器与服务端行为不一致。

## 一、字节、Unicode 与字符编码

- [ ] 能解释文件字节 `[]byte`、Unicode 码点和 Go `string` 之间的区别。
- [ ] 能区分“解码”（文件字节变成 Unicode）和“编码”（Unicode 变回文件字节）。
- [ ] 理解 UTF-8、UTF-16 LE/BE、UTF-32 LE/BE 的基本字节结构。
- [ ] 理解 BOM 的用途，并知道 BOM 是文件内容的一部分，需要在未修改时原样保留。
- [ ] 知道 GBK、GB18030、Big5、Shift JIS 和 Windows-1252 等传统编码的用途。
- [ ] 理解字符集的表示范围，例如 `😀` 可以写入 GB18030，但不能写入 GBK。
- [ ] 知道 `�`（U+FFFD）通常意味着解码器替换了无法解释的字节。
- [ ] 理解编码自动检测是概率判断：短文本和只有 ASCII 的文本可能没有唯一答案。
- [ ] 能说明为什么自动检测必须配合“使用编码重新解码”的手动入口。
- [ ] 能区分“按某编码重新解码原始字节”和“把当前 Unicode 正文保存成某编码”。

练习：

```bash
printf '中文配置\n' | iconv -f UTF-8 -t GB18030 > /tmp/plainmote-gb18030.txt
xxd /tmp/plainmote-gb18030.txt
iconv -f GB18030 -t UTF-8 /tmp/plainmote-gb18030.txt
```

对应代码：

- `internal/store/textencoding.go`
- `internal/store/contenttype.go`

参考资料：

- [Unicode UTF 与 BOM FAQ](https://www.unicode.org/faq/utf_bom.html)
- [WHATWG Encoding Standard](https://encoding.spec.whatwg.org/)

## 二、浏览器文件与表单

- [ ] 会使用 `File.arrayBuffer()` 读取文件原始字节。
- [ ] 理解 `ArrayBuffer` 和 `Uint8Array`，不会先把未知编码的文件转成 JavaScript 字符串。
- [ ] 会使用 `TextDecoder(label, { fatal: true })`，理解严格解码失败与替换字符之间的区别。
- [ ] 理解 `<input type="file">`、`FormData` 和 `multipart/form-data` 的关系。
- [ ] 理解 textarea 和 CodeMirror 保存的是 Unicode 字符串，不保存原始文件字节。
- [ ] 能解释为什么刚上传且未修改时应提交原始 `File`。
- [ ] 能解释为什么编辑后要清空文件输入，让 textarea 成为唯一内容来源。
- [ ] 理解 `formdata` 事件怎样删除或替换即将提交的字段。
- [ ] 能识别文件异步读取中的竞态，例如较早选择的文件晚于新文件读取完成。
- [ ] 理解 CodeMirror 的 document、update listener 和整篇内容替换。

对应代码：

- `internal/web/static/editor.js`
- `internal/web/templates/resource.html`

参考资料：

- [MDN：Blob.arrayBuffer()](https://developer.mozilla.org/en-US/docs/Web/API/Blob/arrayBuffer)
- [MDN：TextDecoder](https://developer.mozilla.org/en-US/docs/Web/API/TextDecoder)
- [MDN：formdata event](https://developer.mozilla.org/en-US/docs/Web/API/HTMLFormElement/formdata_event)

## 三、Go 服务端的文本边界

- [ ] 熟悉 `[]byte`、`string`、`io.Reader` 和 `io.ReadCloser` 的职责。
- [ ] 不会对未知编码的文件直接执行 `string(content)`。
- [ ] 会使用 `golang.org/x/text/encoding` 的 Decoder 和 Encoder。
- [ ] 知道部分解码器会生成 U+FFFD，因此“没有返回 error”不一定代表无损。
- [ ] 能检查解码结果是否像文本，同时避免把已知图片、音视频或压缩文件送入编辑器。
- [ ] 能在保存时拒绝目标编码无法表示的字符，而不是写入问号或替换字符。
- [ ] 理解上传文件与 textarea 内容在服务端为什么要走不同路径。
- [ ] 理解 `nil` 内容表示保留当前对象，空内容和未提交内容不是一回事。
- [ ] 能检查请求大小限制在普通表单和 multipart 上传中是否都有效。

对应代码：

- `internal/store/textencoding.go`
- `internal/store/resources.go`
- `internal/web/resources.go`

参考资料：

- [Go：golang.org/x/text/encoding](https://pkg.go.dev/golang.org/x/text/encoding)
- [Go：golang.org/x/text/transform](https://pkg.go.dev/golang.org/x/text/transform)

## 四、HTTP 媒体类型与字符集

- [ ] 能区分媒体类型 `application/yaml` 与字符集参数 `charset=gb18030`。
- [ ] 理解数据库中的源编码负责编辑和写回，HTTP `charset` 负责响应的读取提示。
- [ ] 知道 BOM 是否存在不能只靠 HTTP `charset` 表达。
- [ ] 理解 `Content-Disposition` 如何提供下载文件名。
- [ ] 理解 `X-Content-Type-Options: nosniff` 的作用。
- [ ] 理解为什么同源服务不能把用户上传的 HTML、SVG 或 JavaScript 直接作为可执行内容返回。
- [ ] 能确认远程资源公开访问保持原始字节，而只读预览按上游 `charset` 或检测结果解码。

对应代码：

- `internal/store/contenttype.go`
- `internal/web/public.go`
- `internal/web/resources.go`

## 五、PostgreSQL 与对象存储

- [ ] 理解正文原始字节保存在 S3 兼容对象存储，PostgreSQL 保存对象 Key 和元数据。
- [ ] 理解 `content_type` 与 `content_encoding` 是两个不同字段。
- [ ] 能沿着创建、查询、列表、更新和分享消费 SQL 检查新增字段是否完整传递。
- [ ] 能检查 schema 默认值对已有行和新行的影响。
- [ ] 理解只修改名称或文件名时为什么不应重写对象，以及编码不能脱离正文单独修改。
- [ ] 理解替换正文时先写新对象、再更新数据库、最后删除旧对象的原因。

对应代码：

- `internal/store/schema.sql`
- `internal/store/models.go`
- `internal/store/resources.go`
- `internal/store/shares.go`

## 六、测试与 review 方法

- [ ] 对未修改文件使用 `bytes.Equal`，验证原始字节完全一致。
- [ ] 对每种编码验证“编码 → 解码 → 编辑 → 再编码”。
- [ ] 覆盖 UTF BOM、无 BOM UTF-16/32、GB18030、GBK、Big5、Shift JIS 和单字节编码代表样本。
- [ ] 覆盖畸形字节、检测失败、二进制内容和目标编码无法表示字符的失败路径。
- [ ] 验证上传后编辑器立即更新，而不是依赖第一次保存后的服务端页面。
- [ ] 验证只修改元数据不会生成新的对象 Key。
- [ ] 验证数据库确实持久化源编码。
- [ ] 验证公开下载的正文仍是预期字节，并返回合适的 Content-Type。
- [ ] 验证远程资源预览能读取上游声明的 charset。
- [ ] 用真实 PostgreSQL 跑集成测试，不能只依赖跳过数据库的单元测试结果。

常用检查命令：

```bash
npm --prefix tools/codemirror test
go vet ./...
PLAINMOTE_TEST_DATABASE_URL='postgres://plainmote:plainmote@127.0.0.1:54329/plainmote?sslmode=disable' go test ./...
PLAINMOTE_TEST_DATABASE_URL='postgres://plainmote:plainmote@127.0.0.1:54329/plainmote?sslmode=disable' go test -race ./...
docker build -t plainmote:review .
```

对应测试：

- `internal/store/contenttype_test.go`
- `internal/web/editor_test.go`
- `internal/web/upload_encoding_test.go`
- `internal/web/remote_test.go`
- `tools/codemirror/encoding_test.mjs`

## 七、依赖、构建与许可证

- [ ] 理解 `package.json` 的直接依赖与 `package-lock.json` 的完整锁定关系。
- [ ] 能用 `tools/codemirror/build.sh` 从干净临时目录重建浏览器 bundle。
- [ ] 确认浏览器运行时不会从第三方 CDN 下载编辑器或编码检测代码。
- [ ] 新增浏览器依赖时检查许可证，并更新 `tools/codemirror/NOTICE.md`。
- [ ] 知道生成后的 bundle 需要检查可复现构建、体积和缓存行为，不按源码行数 review。
- [ ] 修改浏览器和服务端支持的编码集合时，同时更新选项、实现和测试。

## 每次编码相关改动的最终检查表

- [ ] 原始字节在编码确定前没有被转换成字符串。
- [ ] 自动检测结果可以由用户手动覆盖。
- [ ] 解码失败不会产生可保存的乱码文本。
- [ ] 未修改内容保持原始字节和对象 Key。
- [ ] 修改内容按当前选择的编码写回。
- [ ] 只修改保存编码或行尾也会提交编辑器正文，并覆盖仍处于选中状态的上传文件。
- [ ] 无法表示的字符会让保存失败并显示错误。
- [ ] BOM、源编码和 HTTP charset 没有被混为一谈。
- [ ] 浏览器、Go Store、页面选项和数据库字段保持一致。
- [ ] 二进制文件与同源可执行内容仍受限制。
- [ ] 字节级测试、浏览器侧测试、PostgreSQL 集成测试和生产镜像构建全部通过。

完成前六节后，已经可以独立 review 大部分文件编辑改动。字符集检测算法内部、CodeMirror 扩展系统和
依赖打包细节可以在需要修改对应部分时再深入。
