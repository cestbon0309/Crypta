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

struct BrowserView: View {
    @EnvironmentObject var model: AppModel
    let path: String

    @State private var items: [Item] = []
    @State private var loading = true
    @State private var loadError: String?
    @State private var actionError: String?
    @State private var search = ""

    @State private var preview: Item?
    @State private var renaming: Item?
    @State private var moving: Item?
    @State private var deleting: Item?
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
        search.isEmpty ? items : items.filter { $0.name.localizedCaseInsensitiveContains(search) }
    }

    var body: some View {
        content
            .navigationTitle(title)
            .navigationBarTitleDisplayMode(path.isEmpty ? .large : .inline)
            .searchable(text: $search, prompt: "在此文件夹中搜索")
            .toolbar {
                ToolbarItem(placement: .primaryAction) {
                    Menu {
                        Button { pickingPhotos = true } label: { Label("上传照片或视频", systemImage: "photo.on.rectangle") }
                        Button { importingFiles = true } label: { Label("上传文件", systemImage: "doc.badge.plus") }
                        Button { showingJobs = true } label: { Label("离线下载", systemImage: "icloud.and.arrow.down") }
                        Divider()
                        Button { newName = ""; creatingFolder = true } label: { Label("新建文件夹", systemImage: "folder.badge.plus") }
                    } label: {
                        Image(systemName: "plus.circle.fill")
                    }
                    .accessibilityLabel("添加")
                }
            }
            .task { await load() }
            .refreshable { await load(fresh: true) }
            .onChange(of: model.uploadsFinished) { _ in Task { await load(fresh: true) } }
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
            .confirmationDialog("删除“\(deleting?.name ?? "")”？", isPresented: Binding(get: { deleting != nil }, set: { if !$0 { deleting = nil } }), titleVisibility: .visible, presenting: deleting) { item in
                Button("删除", role: .destructive) {
                    Task { await change { try await $0.post("api/delete", ["path": item.path]) } }
                }
            } message: { item in
                Text(item.isDir ? "文件夹和里面的所有内容会被移到 Google Drive 的回收站。" : "文件会被移到 Google Drive 的回收站。")
            }
            .alert("出错了", isPresented: Binding(get: { actionError != nil }, set: { if !$0 { actionError = nil } })) {
                Button("好", role: .cancel) {}
            } message: { Text(actionError ?? "") }
            .sheet(item: $moving) { item in
                FolderPicker(moving: item) { dest in
                    Task { await change { try await $0.post("api/move", ["from": item.path, "to": joinPath(dest, item.name)]) } }
                }
            }
            .fullScreenCover(item: $preview) { PreviewView(item: $0) }
            .overlay(alignment: .bottom) { banner }
            .animation(.default, value: bannerText)
    }

    private var bannerText: String? { busy ?? notice }

    @ViewBuilder private var banner: some View {
        if let text = bannerText {
            HStack(spacing: 8) {
                if busy != nil {
                    ProgressView()
                } else {
                    Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
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
                    Image(systemName: "folder").font(.system(size: 44)).foregroundStyle(.secondary)
                    Text("这里还没有文件").font(.headline)
                    Text("点右上角的 + 上传文件或新建文件夹。").font(.footnote).foregroundStyle(.secondary)
                }
                .frame(maxWidth: .infinity)
                .padding(.top, 120)
            }
        } else {
            List(shown) { item in
                row(item)
            }
            .listStyle(.plain)
        }
    }

    @ViewBuilder private func row(_ item: Item) -> some View {
        Group {
            if item.isDir {
                NavigationLink(value: item) { ItemRow(item: item) }
            } else {
                Button { preview = item } label: { ItemRow(item: item) }
                    .buttonStyle(.plain)
            }
        }
        .contextMenu {
            if !item.isDir {
                Button { Task { await run { try await model.download(item) } } } label: { Label("下载到本机", systemImage: "arrow.down.circle") }
            }
            Button { newName = item.name; renaming = item } label: { Label("重命名", systemImage: "pencil") }
            Button { moving = item } label: { Label("移动", systemImage: "folder") }
            Button(role: .destructive) { deleting = item } label: { Label("删除", systemImage: "trash") }
        }
        .swipeActions(edge: .trailing) {
            Button(role: .destructive) { deleting = item } label: { Label("删除", systemImage: "trash") }
            Button { moving = item } label: { Label("移动", systemImage: "folder") }.tint(.indigo)
        }
        .swipeActions(edge: .leading) {
            if !item.isDir {
                Button { Task { await run { try await model.download(item) } } } label: { Label("下载", systemImage: "arrow.down.circle") }.tint(.blue)
            }
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
        if queued > 0 {
            let text = "已添加 \(queued) 个上传任务，可在“传输”查看进度"
            notice = text
            Task {
                try? await Task.sleep(nanoseconds: 4_000_000_000)
                if notice == text { notice = nil }
            }
        }
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

struct ItemRow: View {
    let item: Item

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: item.symbol)
                .font(.title3)
                .foregroundStyle(item.isDir ? Color.accentColor : .secondary)
                .frame(width: 30)
            VStack(alignment: .leading, spacing: 2) {
                Text(item.name).lineLimit(2)
                Text(item.isDir ? item.modTime.formatted(date: .abbreviated, time: .omitted)
                     : "\(formatSize(item.size)) · \(item.modTime.formatted(date: .abbreviated, time: .omitted))")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Spacer(minLength: 0)
        }
        .contentShape(Rectangle())
        .padding(.vertical, 2)
    }
}

/// Lets the user walk the folder tree and choose where to move an item.
struct FolderPicker: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let moving: Item
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
    let moving: Item
    let onPick: (String) -> Void
    let onCancel: () -> Void
    @State private var folders: [Item] = []
    @State private var loading = true

    private var isSource: Bool { path == parentPath(moving.path) }

    var body: some View {
        List {
            if loading {
                ProgressView().frame(maxWidth: .infinity)
            } else if folders.isEmpty {
                Text("没有子文件夹").foregroundStyle(.secondary)
            }
            ForEach(folders) { f in
                NavigationLink(value: f.path) { Label(f.name, systemImage: "folder") }
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
            folders = all.filter { $0.isDir && $0.path != moving.path }
            loading = false
        }
    }
}
