# zendriver 0.17.1 → dgodriver API 对照

对照基于本目录相邻的 `zendriver-main/zendriver/core` 和 `cdp`。CDP 的所有域、命令、参数、返回结果与事件均由快照生成。高层 API 使用 Go 命名和显式资源管理；此表列出对应入口与有意的语义调整。

| zendriver | dgodriver |
|---|---|
| `start`, `Browser.create`, `Browser.start` | `Start(ctx, Config)` |
| 连接已有浏览器 | `Connect` / `Config.Host + Port` / `WebSocketURL` |
| `Browser.get`, `get_tab`, `main_tab`, `tabs`, `targets` | `Get`, `GetTab`, `MainTab`, `Tabs`, `Targets`, `UpdateTargets` |
| `Browser.create_context` | `CreateContext` → `BrowserContext.NewTab`；context 与 tab 分别管理 |
| `Browser.stop`, `stopped`, `wait`, `sleep`, `test_connection` | `Close`, `Stop(ctx)`, `Stopped`, `Wait`, `Sleep`, `TestConnection` |
| `Browser.websocket_url`, `info` | `WebSocketURL`, `Info` |
| `grant_all_permissions`, `tile_windows` | `GrantAllPermissions`, `GrantPermissions`, `TileWindows`；屏幕尺寸显式传入以支持远程主机 |
| Config 浏览器、headless、sandbox、expert、语言、UA、profile、连接参数 | `Config`；`NoSandbox` 与 `EnableWebRTC` 使用 Go 零值安全的反向选项 |
| Config `add_argument`, `add_extension`, `browser_args` | `AddArgument`, `AddExtension`, `Arguments`；扩展目录 / ZIP / CRX |
| Config `find_executable`, `merge_disable_features_args` | `FindExecutable`, `MergeDisableFeaturesArgs` |
| `Connection.aopen`, `aclose`, `attached`, `closed` | `Dial`, `Close`, `SessionID`, `Closed`, `Done`；Tab 懒加载 Connection |
| `Connection.send`, `feed_cdp` | `Send`, `Call`, typed command `Execute`；异步发送使用 goroutine |
| `add_handler`, `remove_handlers` | `On`, `OnEvent`, `AddHandler`, `Subscription.Close`, `RemoveHandlers` |
| `Connection.wait`, `sleep`, `update_target` | `Wait`, `Sleep`, `Tab.UpdateTarget` |
| `Connection.target` 及 target metadata 属性 | `Tab.Target`、`TargetID`、`URL`、`Title`；其余字段由 typed `TargetTargetInfo` 提供 |
| `ProtocolException` | `ProtocolError`，支持 `errors.As` |
| `Tab.get`, `back`, `forward`, `reload`, `close` | `Get`, `Navigate`, `Back`, `Forward`, `Reload`, `Close`, `CloseContext` |
| `query_selector`, `query_selector_all`, `select`, `select_all` | 同名 PascalCase；`QueryOptions` 控制 timeout / frame；立即查询无匹配返回 `ErrNotFound` |
| `find`, `find_all`, `find_element_by_text`, `find_elements_by_text`, `xpath` | `Find`, `FindAll`, `FindElementByText`, `FindElementsByText`, `XPath`；`Exact` 选择完整文本匹配 |
| `get_frames`, 元素 `get_frame` | `GetFrames`, `Element.GetFrame`；OOPIF 是独立 Tab，in-process frame 使用 `ContentDocument` |
| `wait_for`, `wait_for_ready_state`, `disable_dom_agent` | `WaitFor`, `WaitForReadyState`, `DisableDOMAgent` |
| `evaluate`, `js_dumps`, `get_content` | `Evaluate`, `JSDumps`, `GetContent`；awaitPromise / remoteObject 使用 `EvaluateOptions` |
| `get_window`, `set_window_size`, `set_window_state` | `GetWindow`, `SetWindowSize`, `SetWindowState` |
| `maximize`, `minimize`, `fullscreen`, `medimize` | `Maximize`, `Minimize`, `Fullscreen`, `Medimize` |
| `activate`, `bring_to_front` | `Activate`, `BringToFront` |
| `inspector_url`, `inspector_open`, `open_external_inspector` | `InspectorURL`, `OpenExternalInspector` |
| `scroll_down`, `scroll_up`, `is_scrolled_to_bottom` | `ScrollDown`, `ScrollUp`, `IsScrolledToBottom`；滚动量为视口高度百分比，speed 是手势速度 |
| `mouse_move`, `mouse_down`, `mouse_up`, `mouse_click`, `mouse_drag`, `flash_point` | 同名 PascalCase；鼠标状态跟随 target；拖动使用源 / 目标坐标 |
| `bypass_insecure_connection_warning` | `BypassInsecureConnectionWarning` |
| `expect_request`, `expect_response` | `ExpectRequest`, `ExpectResponse` → `WaitRequest`, `WaitResponse`, `Body`, `Reset`, `Close` |
| `intercept`、`request`、`response_body` | `Intercept` → `Next` → `InterceptedRequest.Event` / `Body`；一个 interceptor 可消费多次 |
| `fail_request`, `continue_request`, `fulfill_request`, `continue_response` | `Fail`, `Continue`, `Fulfill`, `ContinueResponse`；Body 使用字节，不要求调用者做 Base64 |
| `expect_download`, `set_download_path`, `download_file` | `ExpectDownload`, `WaitBegin` / `Wait`, `SetDownloadPath`, `DownloadFile`；等待完成可获取最终文件路径 |
| `screenshot_b64`, `save_screenshot`, `print_to_pdf`, `save_snapshot` | `ScreenshotBase64`, `Screenshot`, `SaveScreenshot`, `PrintToPDF`, `SaveSnapshot`；截图默认 PNG |
| `get_all_linked_sources`, `get_all_urls` | `GetAllLinkedSources` 返回元素；`GetAllURLs` 返回 URL |
| `get_local_storage`, `set_local_storage`, `set_user_agent` | `GetLocalStorage`, `SetLocalStorage`, `SetUserAgent` |
| CookieJar `get_all`, `set_all`, `clear`, `save`, `load` | `GetAll`, `SetAll`, `Clear`, `Save`, `Load`, `HTTPCookies`；JSON 替代 pickle |
| Element node 的所有元数据属性 | `Node()` 返回完整 `cdp.DOMNode`，含 shadow / frame / document / pseudo metadata |
| Element `tag`, `tag_name`, `attributes`, `attrs`, `get`, `tab` | `TagName`, `Attributes`, `Get`, `Tab` |
| Element `parent`, `children`, `shadow_roots`, `shadow_children`, `content_document` | `Parent`, `Children`, `ShadowRoots`, `ShadowChildren`, `ContentDocument` |
| Element `update`, `tree` | `Update`, `Node`；完整树由 CDP `DOM.GetDocument` 提供 |
| Element `remote_object`, `object_id`, `apply` | `RemoteObject`, `ObjectID`, `Apply`；Apply 自动释放临时 object handle，显式返回的 handle 由调用者释放 |
| Element `click`, `focus`, `scroll_into_view`, `get_position` | `Click`, `Focus`, `ScrollIntoView`, `GetPosition`；`Position.Center` / `ToViewport` |
| Element `mouse_click`, `mouse_move`, `mouse_drag` | `MouseClick`, `MouseMove`, `MouseDrag`；任意坐标 / relative drag 使用 Tab 方法 |
| Element `clear_input`, `clear_input_by_deleting`, `send_keys`, `send_file` | `ClearInput`, `ClearInputByDeleting`, `SendKeys`, `PressKey`, `SendFile` |
| Element `select_option`, `set_value`, `set_text`, `get_html`, `text`, `text_all` | `SelectOption`, `SetValue`, `SetText`, `GetHTML`, `Text`, `TextAll`；SetValue 同步受控输入的 value 和事件 |
| Element `get_js_attributes`, `save_to_dom`, `remove_from_dom` | `GetJSAttributes`, `SaveToDOM`, `RemoveFromDOM`, `SetAttribute`, `RemoveAttribute` |
| Element `query_selector`, `query_selector_all` | `QuerySelector`, `QuerySelectorAll` |
| Element 截图 / flash / highlight / recording | `ScreenshotBase64`, `Screenshot`, `SaveScreenshot`, `Flash`, `HighlightOverlay`, `RecordVideo`, `IsRecording`, `VideoRecording.Stop` |
| `SpecialKeys`, `KeyModifiers`, `KeyEvents` | `PressKey` 的标准 key 字符串、`Alt/Control/Meta/Shift`、`KeyInput`、`TypeText`、`DispatchKeyEvents`；原始 payload 使用 typed CDP |
| `ProxyForwarder`, `UpstreamProxy` | `NewProxyForwarder`, `ProxyServer`, `Close`；HTTP / TLS / SOCKS 握手内部实现 |
| `cf_find_interactive_challenge`, `cf_wait_for_interactive_challenge`, `cf_is_interactive_challenge_present`, `verify_cf` | `FindCloudflareChallenge`, `WaitForCloudflareChallenge`, `IsCloudflareChallengePresent`, `VerifyCF`；线上挑战未验证 |
| DOM / frame utility、`circle`、`compare_target_info` | `WalkNodes`, `FilterNodes`, `FlattenFrameTree`, `Circle`, `CompareTargetInfo` |
| Python event loop、动态 dict、atexit registry、undetected_chromedriver adapter | Go context / struct / map / defer；已有实例使用公开 CDP WebSocket 接入 |

## 验证边界

协议覆盖数量由快照测试检查，生成文件可离线重建。高层核心流程有本地 Chrome 集成测试；代理使用本地 HTTP / SOCKS 服务验证。不同 Chromium 版本对实验域、DRM、扩展、显示设备和屏幕捕获的支持由浏览器决定。Cloudflare 的真实挑战和带显示器的屏幕捕获没有在本环境验证，不宣称所有站点都能通过。
