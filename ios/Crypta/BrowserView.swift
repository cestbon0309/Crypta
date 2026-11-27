import PhotosUI
import SwiftUI
import UniformTypeIdentifiers

/// A photo or video picked from the library, copied to a temporary file.
struct PickedMedia: Transferable {
    let url: URL

    static var transferRepresentation: some TransferRepresentation {
        FileRepresentation(importedContentType: .movie) { try PickedMedia(copying: $0.file) }
        FileRepresentation(importedContentType: .image) { try PickedMedia(copying: $0.file) }
    }

    init(copying source: URL) throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        url = dir.appendingPathComponent(source.lastPathComponent)
        try FileManager.default.copyItem(at: source, to: url)
    }
}

/// The system file picker. It hands over copies of the chosen files, so
/// files in iCloud Drive or another app need no further permission.
struct DocumentPicker: UIViewControllerRepresentable {
    let onPick: ([URL]) -> Void

    func makeCoordinator() -> Coordinator { Coordinator(onPick: onPick) }

    func makeUIViewController(context: Context) -> UIDocumentPickerViewController {
        let picker = UIDocumentPickerViewController(forOpeningContentTypes: [.item], asCopy: true)
        picker.allowsMultipleSelection = true
        picker.delegate = context.coordinator
        return picker
    }

    func updateUIViewController(_ picker: UIDocumentPickerViewController, context: Context) {}

    final class Coordinator: NSObject, UIDocumentPickerDelegate {
        let onPick: ([URL]) -> Void
        init(onPick: @escaping ([URL]) -> Void) { self.onPick = onPick }

        func documentPicker(_ controller: UIDocumentPickerViewController, didPickDocumentsAt urls: [URL]) {
            onPick(urls)
        }
    }
}

/// The folder stack of the Files tab, so a view can jump back several levels.
final class BrowserNav: ObservableObject {
    @Published var path: [Item] = []

    func pop(to depth: Int) {
        if depth < path.count { path.removeLast(path.count - depth) }
    }
}

struct MoveRequest: Identifiable {
    let id = UUID()
    let items: [Item]
}

struct BrowserView: View {
    @EnvironmentObject var model: AppModel
    @EnvironmentObject var nav: BrowserNav
    let path: String

    @AppStorage(Prefs.viewMode) private var mode = ViewMode.details
    @AppStorage(Prefs.sortKey) private var sortKey = SortKey.name
    @AppStorage(Prefs.sortAscending) private var sortAscending = true

    @State private var items: [Item] = []
    @State private var loading = true
    @State private var loadError: String?
    @State private var actionError: String?
    @State private var search = ""

    @State private var selecting = false
    @State private var selection = Set<String>()

    @State private var preview: Item?
    @State private var info: Item?
    @State private var renaming: Item?
    @State private var moving: MoveRequest?
    @State private var deleting: [Item] = []
    @State private var newName = ""
    @State private var creatingFolder = false
    @State private var importingFiles = false
    @State private var pickingPhotos = false
    @State private var photos: [PhotosPickerItem] = []
    @State private var busy: String?
    @State private var notice: String?
    @State private var showingJobs = false

    private var title: String { path.isEmpty ? "Crypta" : (path as NSString).lastPathComponent }
    private var shown: [Item] {
        let found = search.isEmpty ? items : items.filter { $0.name.localizedCaseInsensitiveContains(search) }
        return sortKey.sorted(found, ascending: sortAscending)
    }
    private var selectedItems: [Item] { items.filter { selection.contains($0.id) } }

    var body: some View {
        dialogs
            .navigationTitle(selecting ? "已选择 \(selection.count) 项" : title)
            .navigationBarTitleDisplayMode(path.isEmpty && !selecting ? .large : .inline)
            .navigationBarBackButtonHidden(selecting)
            .searchable(text: $search, placement: .navigationBarDrawer(displayMode: .always), prompt: "在此文件夹中搜索")
            .toolbar { toolbar }
            .toolbar(selecting ? .hidden : .automatic, for: .tabBar)
            .task { await load() }
            .refreshable { await load(fresh: true) }
            .onChange(of: model.uploadsFinished) { _ in Task { await load(fresh: true) } }
            .onChange(of: items) { now in selection.formIntersection(now.map(\.id)) }
            .onDisappear { endSelecting() }
    }

    @ToolbarContentBuilder private var toolbar: some ToolbarContent {
        if selecting {
            ToolbarItem(placement: .navigationBarLeading) {
                let all = Set(shown.map(\.id))
                Button(selection.isSuperset(of: all) && !all.isEmpty ? "取消全选" : "全选") {
                    selection = selection.isSuperset(of: all) ? [] : all
                }
            }
            ToolbarItem(placement: .confirmationAction) { Button("完成") { endSelecting() } }
            ToolbarItemGroup(placement: .bottomBar) {
                let picked = selectedItems
                Button { downloadSelected() } label: { Label("下载", systemImage: "arrow.down.circle") }
                    .disabled(!picked.contains { !$0.isDir })
                Spacer()
                Button { moving = MoveRequest(items: picked) } label: { Label("移动", systemImage: "folder") }
                    .disabled(picked.isEmpty)
                Spacer()
                Button(role: .destructive) { deleting = picked } label: { Label("删除", systemImage: "trash") }
                    .disabled(picked.isEmpty)
            }
        } else {
            ToolbarItemGroup(placement: .primaryAction) {
                Menu {
                    Button { selecting = true } label: { Label("选择", systemImage: "checkmark.circle") }
                        .disabled(items.isEmpty)
                    Picker("显示方式", selection: $mode) {
                        ForEach(ViewMode.allCases) { Label($0.title, systemImage: $0.symbol).tag($0) }
                    }
                    Menu {
                        Picker("排序方式", selection: $sortKey) {
                            ForEach(SortKey.allCases) { Text($0.title).tag($0) }
                        }
                        Picker("顺序", selection: $sortAscending) {
                            Text("升序").tag(true)
                            Text("降序").tag(false)
                        }
                    } label: { Label("排序：\(sortKey.title)", systemImage: "arrow.up.arrow.down") }
                } label: {
                    Image(systemName: "ellipsis.circle")
                }
                .accessibilityLabel("显示与选择")
                Menu {
                    Button { pickingPhotos = true } label: { Label("上传照片或视频", systemImage: "photo.on.rectangle") }
                    Button { importingFiles = true } label: { Label("上传文件", systemImage: "doc.badge.plus") }
                    Button { showingJobs = true } label: { Label("离线下载", systemImage: "icloud.and.arrow.down") }
                    Divider()
                    Button { newName = ""; creatingFolder = true } label: { Label("新建文件夹", systemImage: "folder.badge.plus") }
                } label: {
                    Image(systemName: "plus")
                }
                .accessibilityLabel("添加")
            }
        }
    }

    private var pickers: some View {
        content
            .photosPicker(isPresented: $pickingPhotos, selection: $photos, matching: .any(of: [.images, .videos]))
            .onChange(of: photos) { picked in
                guard !picked.isEmpty else { return }
                photos = []
                Task { await upload(picked.count, from: picked) { try await $0.media() } }
            }
            .sheet(isPresented: $importingFiles) {
                DocumentPicker { urls in
                    importingFiles = false
                    Task { await upload(urls.count, from: urls) { $0 } }
                }
                .ignoresSafeArea()
            }
            .navigationDestination(isPresented: $showingJobs) { ServerJobsView(folder: path) }
            .sheet(item: $moving) { request in
                FolderPicker(moving: request.items) { dest in
                    Task { await changeEach(request.items, "正在移动") { api, item in
                        try await api.post("api/move", ["from": item.path, "to": joinPath(dest, item.name)])
                    } }
                }
            }
            .sheet(item: $info) { ItemInfoView(item: $0) }
            .fullScreenCover(item: $preview) { PreviewView(item: $0) }
            .overlay(alignment: .bottom) { banner }
            .animation(.default, value: bannerText)
    }

    private var dialogs: some View {
        pickers
            .alert("新建文件夹", isPresented: $creatingFolder) {
                TextField("名称", text: $newName)
                Button("取消", role: .cancel) {}
                Button("创建") {
                    let name = newName.trimmingCharacters(in: .whitespaces)
                    guard !name.isEmpty else { return }
                    Task { await change { try await $0.post("api/mkdir", ["path": joinPath(path, name)]) } }
                }
            }
            .alert("重命名", isPresented: Binding(get: { renaming != nil }, set: { if !$0 { renaming = nil } }), presenting: renaming) { item in
                TextField("名称", text: $newName)
                Button("取消", role: .cancel) {}
                Button("好") {
                    let name = newName.trimmingCharacters(in: .whitespaces)
                    guard !name.isEmpty, name != item.name, !name.contains("/") else { return }
                    Task { await change { try await $0.post("api/move", ["from": item.path, "to": joinPath(path, name)]) } }
                }
            }
            .confirmationDialog(deleteTitle, isPresented: Binding(get: { !deleting.isEmpty }, set: { if !$0 { deleting = [] } }), titleVisibility: .visible, presenting: deleting) { targets in
                Button("删除", role: .destructive) {
                    Task { await changeEach(targets, "正在删除") { api, item in try await api.post("api/delete", ["path": item.path]) } }
                }
            } message: { targets in
                Text(targets.contains(where: \.isDir) ? "文件夹和里面的所有内容会被移到 Google Drive 的回收站。" : "文件会被移到 Google Drive 的回收站。")
            }
            .alert("出错了", isPresented: Binding(get: { actionError != nil }, set: { if !$0 { actionError = nil } })) {
                Button("好", role: .cancel) {}
            } message: { Text(actionError ?? "") }
    }

    private var deleteTitle: String {
        deleting.count == 1 ? "删除“\(deleting[0].name)”？" : "删除这 \(deleting.count) 项？"
    }

    private var bannerText: String? { busy ?? notice }

    @ViewBuilder private var banner: some View {
        if let text = bannerText {
            HStack(spacing: 8) {
                if busy != nil {
                    ProgressView()
                } else {
                    Image(systemName: "checkmark.circle.fill").foregroundStyle(.tint)
                }
                Text(text)
            }
            .font(.subheadline)
            .padding(.horizontal, 16).padding(.vertical, 10)
            .background(.regularMaterial, in: Capsule())
            .shadow(color: .black.opacity(0.15), radius: 8, y: 2)
            .padding(.bottom, 12)
            .transition(.move(edge: .bottom).combined(with: .opacity))
        }
    }

    @ViewBuilder private var content: some View {
        if loading && items.isEmpty {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        } else if let loadError, items.isEmpty {
            VStack(spacing: 12) {
                Image(systemName: "wifi.exclamationmark").font(.largeTitle).foregroundStyle(.secondary)
                Text("无法载入").font(.headline)
                Text(loadError).font(.footnote).foregroundStyle(.secondary).multilineTextAlignment(.center)
                Button("重试") { Task { await load(fresh: true) } }.buttonStyle(.bordered)
            }
            .padding()
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        } else if items.isEmpty {
            ScrollView {
                VStack(spacing: 10) {
                    Image(systemName: "folder").font(.system(size: 44)).foregroundStyle(.tertiary)
                    Text("这里还没有文件").font(.headline)
                    Text("点右上角的 + 上传文件或新建文件夹。").font(.footnote).foregroundStyle(.secondary)
                }
                .frame(maxWidth: .infinity)
                .padding(.top, 120)
            }
            .safeAreaInset(edge: .top, spacing: 0) { pathBar }
        } else {
            Group {
                if mode == .icons { grid } else { list }
            }
            .safeAreaInset(edge: .top, spacing: 0) { pathBar }
        }
    }

    @ViewBuilder private var pathBar: some View {
        if !path.isEmpty && !selecting {
            PathBar(path: path) { nav.pop(to: $0) }
        }
    }

    private var summary: some View {
        let folders = shown.filter(\.isDir).count
        let files = shown.count - folders
        let bytes = shown.reduce(Int64(0)) { $0 + ($1.isDir ? 0 : $1.size) }
        var parts: [String] = []
        if folders > 0 { parts.append("\(folders) 个文件夹") }
        if files > 0 { parts.append("\(files) 个文件，共 \(formatSize(bytes))") }
        return Text(parts.isEmpty ? "没有匹配的项目" : parts.joined(separator: " · "))
            .font(.footnote)
            .foregroundStyle(.secondary)
            .frame(maxWidth: .infinity)
            .padding(.vertical, 14)
    }

    private var list: some View {
        List {
            ForEach(shown) { item in
                entry(item) { ItemRow(item: item, detailed: mode == .details, selected: selecting ? selection.contains(item.id) : nil) }
                    // Not a destructive button: that one takes the row away
                    // at once, before the deletion is confirmed.
                    .swipeActions(edge: .trailing, allowsFullSwipe: false) {
                        if !selecting {
                            Button { deleting = [item] } label: { Image(systemName: "trash") }
                                .tint(.red)
                                .accessibilityLabel("删除")
                        }
                    }
            }
            summary.listRowSeparator(.hidden)
        }
        .listStyle(.plain)
    }

    private var grid: some View {
        ScrollView {
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 92), spacing: 8, alignment: .top)], spacing: 10) {
                ForEach(shown) { item in
                    entry(item) { ItemCell(item: item, selected: selecting ? selection.contains(item.id) : nil) }
                }
            }
            .padding(.horizontal, 12)
            .padding(.top, 10)
            summary
        }
    }

    /// One item: a tap opens it (or picks it while selecting), a long press
    /// shows what can be done with it.
    @ViewBuilder private func entry<Label: View>(_ item: Item, @ViewBuilder label: () -> Label) -> some View {
        if selecting {
            Button { selection.formSymmetricDifference([item.id]) } label: { label() }
                .buttonStyle(.plain)
        } else {
            Group {
                if item.isDir && mode != .icons {
                    NavigationLink(value: item) { label() }
                } else if item.isDir {
                    NavigationLink(value: item) { label() }.buttonStyle(.plain)
                } else {
                    Button { preview = item } label: { label() }.buttonStyle(.plain)
                }
            }
            .contextMenu { menu(for: item) }
        }
    }

    @ViewBuilder private func menu(for item: Item) -> some View {
        Button { info = item } label: { Label("查看详细信息", systemImage: "info.circle") }
        if !item.isDir {
            Button { download(item) } label: { Label("下载到本机", systemImage: "arrow.down.circle") }
        }
        Button { newName = item.name; renaming = item } label: { Label("重命名", systemImage: "pencil") }
        Button { moving = MoveRequest(items: [item]) } label: { Label("移动", systemImage: "folder") }
        Button { selecting = true; selection = [item.id] } label: { Label("选择", systemImage: "checkmark.circle") }
        Divider()
        Button(role: .destructive) { deleting = [item] } label: { Label("删除", systemImage: "trash") }
    }

    private func endSelecting() {
        selecting = false
        selection = []
    }

    private func show(_ text: String) {
        notice = text
        Task {
            try? await Task.sleep(nanoseconds: 4_000_000_000)
            if notice == text { notice = nil }
        }
    }

    private func download(_ item: Item) {
        Task {
            await run { try await model.download(item) }
            if actionError == nil { show("已添加下载任务，可在“传输”查看进度") }
        }
    }

    private func downloadSelected() {
        let files = selectedItems.filter { !$0.isDir }
        endSelecting()
        Task {
            var queued = 0
            for f in files {
                await run { try await model.download(f); queued += 1 }
            }
            if queued > 0 { show("已添加 \(queued) 个下载任务，可在“传输”查看进度") }
        }
    }

    private func load(fresh: Bool = false) async {
        guard let api = model.api else { return }
        do {
            items = try await api.list(path, fresh: fresh)
            loadError = nil
        } catch is CancellationError {
        } catch let e as URLError where e.code == .cancelled {
        } catch {
            loadError = error.localizedDescription
        }
        loading = false
    }

    private func run(_ work: () async throws -> Void) async {
        do { try await work() } catch { actionError = error.localizedDescription }
    }

    private func change(_ work: @escaping (API) async throws -> API.Empty) async {
        guard let api = model.api else { return }
        await run { _ = try await work(api) }
        await load(fresh: true)
    }

    /// Applies one change to each item in turn, then reloads. It carries on
    /// past a failure and reports the last one.
    private func changeEach(_ targets: [Item], _ verb: String, _ work: @escaping (API, Item) async throws -> API.Empty) async {
        guard let api = model.api else { return }
        endSelecting()
        var failure: String?
        for (i, item) in targets.enumerated() {
            if targets.count > 1 { busy = "\(verb) \(i + 1)/\(targets.count)…" }
            do { _ = try await work(api, item) } catch { failure = error.localizedDescription }
        }
        busy = nil
        await load(fresh: true)
        if let failure { actionError = failure }
    }

    /// Queues picked items for upload, saying so as soon as they are picked
    /// and again once the upload jobs exist.
    private func upload<T>(_ count: Int, from picked: [T], file: (T) async throws -> URL) async {
        var queued = 0
        var failure: String?
        for (i, p) in picked.enumerated() {
            busy = "正在准备上传 \(i + 1)/\(count)…"
            do {
                try await model.upload(file: try await file(p), to: path, move: true)
                queued += 1
            } catch {
                failure = error.localizedDescription
            }
        }
        busy = nil
        if queued > 0 { show("已添加 \(queued) 个上传任务，可在“传输”查看进度") }
        if let failure {
            // An alert shown while the picker is still closing is dropped.
            try? await Task.sleep(nanoseconds: 700_000_000)
            actionError = failure
        }
    }
}

private extension PhotosPickerItem {
    func media() async throws -> URL {
        guard let media = try await loadTransferable(type: PickedMedia.self) else {
            throw APIError(message: "无法读取所选的项目")
        }
        return media.url
    }
}

/// Lets the user walk the folder tree and choose where to move an item.
struct FolderPicker: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let moving: [Item]
    let onPick: (String) -> Void

    private func pick(_ dest: String) {
        dismiss()
        onPick(dest)
    }

    var body: some View {
        NavigationStack {
            FolderLevel(path: "", moving: moving, onPick: pick, onCancel: { dismiss() })
                .navigationDestination(for: String.self) { p in
                    FolderLevel(path: p, moving: moving, onPick: pick, onCancel: { dismiss() })
                }
        }
    }
}

private struct FolderLevel: View {
    @EnvironmentObject var model: AppModel
    let path: String
    let moving: [Item]
    let onPick: (String) -> Void
    let onCancel: () -> Void
    @State private var folders: [Item] = []
    @State private var loading = true

    private var isSource: Bool { moving.contains { path == parentPath($0.path) } }

    var body: some View {
        List {
            if loading {
                ProgressView().frame(maxWidth: .infinity)
            } else if folders.isEmpty {
                Text("没有子文件夹").foregroundStyle(.secondary)
            }
            ForEach(folders) { f in
                NavigationLink(value: f.path) { ItemRow(item: f, detailed: false) }
            }
        }
        .navigationTitle(path.isEmpty ? "Crypta" : (path as NSString).lastPathComponent)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .cancellationAction) { Button("取消") { onCancel() } }
            ToolbarItem(placement: .bottomBar) {
                Button("移动到这里") { onPick(path) }
                    .buttonStyle(.borderedProminent)
                    .disabled(isSource)
            }
        }
        .task {
            let all = (try? await model.api?.list(path)) ?? []
            // A folder cannot be moved into itself.
            let sources = Set(moving.map(\.path))
            folders = all.filter { $0.isDir && !sources.contains($0.path) }
            loading = false
        }
    }
}
