import QuickLook
import SwiftUI

struct TransferRow: View {
    let t: Transfer
    let act: (String) -> Void

    private var status: String {
        switch t.state {
        case "queued": return "排队中"
        case "running":
            var s = t.kind == "download" ? "下载中" : (t.phase == "downloading" ? "下载到服务器" : (t.phase == "uploading" ? "加密上传到网盘" : "上传中"))
            s += " · \(formatSize(t.done)) / \(formatSize(t.size))"
            if t.speed > 0 { s += " · \(formatSize(t.speed))/s" }
            return s
        case "paused": return "已暂停 · \(formatSize(t.done)) / \(formatSize(t.size))"
        case "done": return "已完成 · \(formatSize(t.size))"
        case "failed": return "失败：\(t.error ?? "")"
        case "canceled": return "已取消"
        default: return t.state
        }
    }

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: t.kind == "download" ? "arrow.down.circle" : (t.kind == "url" || t.kind == "pikpak" ? "link.circle" : "arrow.up.circle"))
                .font(.title2)
                .foregroundStyle(t.state == "failed" ? .red : (t.state == "done" ? .green : .accentColor))
            VStack(alignment: .leading, spacing: 4) {
                Text(t.name.isEmpty ? t.remote : t.name).lineLimit(1)
                if t.state == "running" || t.state == "paused" {
                    ProgressView(value: t.fraction)
                }
                Text(status).font(.caption).foregroundStyle(t.state == "failed" ? .red : .secondary).lineLimit(3)
            }
            Spacer(minLength: 0)
            switch t.state {
            case "running", "queued":
                Button { act("pause") } label: { Image(systemName: "pause.circle").font(.title2) }
                    .buttonStyle(.borderless).accessibilityLabel("暂停")
            case "paused":
                Button { act("resume") } label: { Image(systemName: "play.circle").font(.title2) }
                    .buttonStyle(.borderless).accessibilityLabel("继续")
            case "failed":
                Button { act("resume") } label: { Image(systemName: "arrow.clockwise.circle").font(.title2) }
                    .buttonStyle(.borderless).accessibilityLabel("重试")
            default: EmptyView()
            }
        }
        .padding(.vertical, 2)
        .swipeActions(allowsFullSwipe: false) {
            Button { act("remove") } label: { Image(systemName: finished ? "trash" : "xmark") }
                .tint(.red)
                .accessibilityLabel(finished ? "移除" : "取消")
        }
    }

    private var finished: Bool { t.state == "done" || t.state == "canceled" }
}

/// This phone's uploads and downloads, and the server's offline downloads.
struct TransfersView: View {
    enum Tab: String, CaseIterable, Identifiable {
        case uploads = "上传"
        case downloads = "下载"
        var id: String { rawValue }
    }

    @EnvironmentObject var model: AppModel
    @EnvironmentObject var store: TransferStore
    @State private var tab = Tab.uploads
    @State private var opened: URL?

    private var uploads: [Transfer] { store.local.filter { $0.kind != "download" } }
    private var downloads: [Transfer] { store.local.filter { $0.kind == "download" } }
    private var paired: Bool { !model.setup.server.isEmpty }
    /// Finished rows of the tab in view, and the API each belongs to.
    private var cleared: [(Transfer, API?)] {
        let rows = tab == .uploads ? uploads.map { ($0, model.api) }
            : downloads.map { ($0, model.api) } + store.server.map { ($0, model.server) }
        return rows.filter { $0.0.state == "done" || $0.0.state == "canceled" }
    }

    var body: some View {
        VStack(spacing: 0) {
            Picker("类型", selection: $tab) {
                ForEach(Tab.allCases) { Text($0.rawValue).tag($0) }
            }
            .pickerStyle(.segmented)
            .padding(.horizontal).padding(.vertical, 8)
            if tab == .uploads { uploadList } else { downloadList }
        }
        .navigationTitle("传输")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            if !cleared.isEmpty {
                Button("清除已完成") {
                    let rows = cleared
                    Task {
                        for (t, api) in rows { try? await api?.act(t.id, "remove") }
                        await model.pollTransfers()
                        await model.pollServer()
                    }
                }
            }
        }
        .quickLookPreview($opened)
        .task {
            while !Task.isCancelled {
                await model.pollServer()
                try? await Task.sleep(nanoseconds: 2_000_000_000)
            }
        }
    }

    @ViewBuilder private var uploadList: some View {
        if uploads.isEmpty {
            empty("arrow.up.circle", "没有上传任务", "在“文件”页右上角的 + 里上传。App 被关掉后，重新打开会从中断的地方继续。")
        } else {
            List(uploads) { row($0, model.api) }.listStyle(.plain)
        }
    }

    @ViewBuilder private var downloadList: some View {
        if downloads.isEmpty && !paired {
            empty("arrow.down.circle", "没有下载任务", "长按文件选“下载到本机”。和服务器配对后，离线下载的任务也显示在这里。")
        } else {
            List {
                if paired {
                    Section("下载到本机") {
                        if downloads.isEmpty { Text("没有任务").foregroundStyle(.secondary) }
                        ForEach(downloads) { row($0, model.api) }
                    }
                    Section("离线下载（服务器）") {
                        if !store.serverReachable {
                            Text("连接不上服务器").foregroundStyle(.secondary)
                        } else if store.server.isEmpty {
                            Text("没有任务").foregroundStyle(.secondary)
                        }
                        ForEach(store.server) { row($0, model.server) }
                    }
                } else {
                    ForEach(downloads) { row($0, model.api) }
                }
            }
            .listStyle(.plain)
        }
    }

    private func row(_ t: Transfer, _ api: API?) -> some View {
        TransferRow(t: t) { action in
            Task {
                try? await api?.act(t.id, action)
                await model.pollTransfers()
                await model.pollServer()
            }
        }
        .contentShape(Rectangle())
        .onTapGesture {
            if t.kind == "download", t.state == "done", let local = t.local {
                let url = local.hasPrefix("/") ? URL(fileURLWithPath: local) : Paths.documents.appendingPathComponent(local)
                if FileManager.default.fileExists(atPath: url.path) { opened = url }
            }
        }
    }

    private func empty(_ symbol: String, _ title: String, _ text: String) -> some View {
        VStack(spacing: 10) {
            Image(systemName: symbol).font(.system(size: 40)).foregroundStyle(.secondary)
            Text(title).font(.headline)
            Text(text).font(.footnote).foregroundStyle(.secondary).multilineTextAlignment(.center)
        }
        .padding(40)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

/// Adds offline download jobs to the paired server.
struct ServerJobsView: View {
    @EnvironmentObject var model: AppModel
    @EnvironmentObject var store: TransferStore
    @State private var link = ""
    @State private var folder: String
    @State private var error: String?
    @State private var added = false

    init(folder: String = "") { _folder = State(initialValue: folder) }

    var body: some View {
        if model.setup.server.isEmpty {
            List {
                Section {
                    NavigationLink { PairView() } label: { Label("与服务器配对", systemImage: "qrcode.viewfinder") }
                } footer: {
                    Text("离线下载由服务器完成：先和服务器配对，之后就能在这里添加下载链接。")
                }
            }
            .navigationTitle("离线下载")
            .navigationBarTitleDisplayMode(.inline)
        } else {
            jobList
        }
    }

    private var jobList: some View {
        List {
            Section {
                TextField("下载链接 https://… 或 pikpak:/网盘路径", text: $link)
                    .keyboardType(.URL).textInputAutocapitalization(.never).autocorrectionDisabled()
                TextField("保存到（网盘内文件夹，可留空）", text: $folder)
                    .textInputAutocapitalization(.never).autocorrectionDisabled()
                Button("添加离线下载") { add() }
                    .disabled(!Self.accepts(link.trimmingCharacters(in: .whitespaces)))
            } footer: {
                if let error { Text(error).foregroundStyle(.red) }
                else if added { Text("已添加，进度在“传输”页的“下载”里。") }
                else { Text("服务器下载完成后会直接加密上传到 Google Drive，手机不用保持连接。进度在“传输”页的“下载”里。") }
            }
        }
        .navigationTitle("离线下载")
        .navigationBarTitleDisplayMode(.inline)
    }

    /// A link, or a file or folder of the PikPak account set up on the server.
    private static func accepts(_ link: String) -> Bool {
        link.hasPrefix("pikpak:") && link.count > 7 || URL(string: link)?.scheme?.hasPrefix("http") == true
    }

    private func add() {
        guard let server = model.server else { return }
        let url = link.trimmingCharacters(in: .whitespaces)
        Task {
            do {
                let _: Transfer = try await server.post("api/transfers", ["kind": "url", "url": url, "remote": folder])
                link = ""
                error = nil
                added = true
                await model.pollServer()
            } catch let e {
                error = e.localizedDescription
            }
        }
    }
}
