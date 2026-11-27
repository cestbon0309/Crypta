import QuickLook
import SwiftUI

struct TransferRow: View {
    let t: Transfer
    let act: (String) -> Void

    private var status: String {
        switch t.state {
        case "queued": return "排队中"
        case "running":
            var s = t.kind == "download" ? "下载中" : (t.phase == "downloading" ? "下载到服务器" : "上传中")
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
            Image(systemName: t.kind == "download" ? "arrow.down.circle" : (t.kind == "url" ? "link.circle" : "arrow.up.circle"))
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
        .swipeActions {
            Button(role: .destructive) { act("remove") } label: {
                Label(t.state == "done" || t.state == "canceled" ? "移除" : "取消", systemImage: "xmark")
            }
        }
    }
}

struct TransfersView: View {
    @EnvironmentObject var model: AppModel
    @State private var opened: URL?

    var body: some View {
        Group {
            if model.transfers.isEmpty {
                VStack(spacing: 10) {
                    Image(systemName: "arrow.up.arrow.down").font(.system(size: 40)).foregroundStyle(.secondary)
                    Text("没有传输任务").font(.headline)
                    Text("上传和下载会显示在这里。App 被关掉后，重新打开会从中断的地方继续。")
                        .font(.footnote).foregroundStyle(.secondary).multilineTextAlignment(.center)
                }.padding(40)
            } else {
                List(model.transfers) { t in
                    TransferRow(t: t) { action in
                        Task {
                            try? await model.api?.act(t.id, action)
                            await model.pollTransfers()
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
                .listStyle(.plain)
            }
        }
        .navigationTitle("传输")
        .toolbar {
            if model.transfers.contains(where: { !$0.isActive && $0.state != "paused" && $0.state != "failed" }) {
                Button("清除已完成") {
                    Task {
                        let _: API.Empty? = try? await model.api?.post("api/transfers/clear")
                        await model.pollTransfers()
                    }
                }
            }
        }
        .quickLookPreview($opened)
    }
}

/// Offline download jobs running on the paired server.
struct ServerJobsView: View {
    @EnvironmentObject var model: AppModel
    @State private var jobs: [Transfer] = []
    @State private var link = ""
    @State private var folder: String
    @State private var error: String?
    @State private var reachable = true

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
                TextField("下载链接 https://…", text: $link)
                    .keyboardType(.URL).textInputAutocapitalization(.never).autocorrectionDisabled()
                TextField("保存到（网盘内文件夹，可留空）", text: $folder)
                    .textInputAutocapitalization(.never).autocorrectionDisabled()
                Button("添加离线下载") { add() }
                    .disabled(URL(string: link.trimmingCharacters(in: .whitespaces))?.scheme?.hasPrefix("http") != true)
            } footer: {
                if let error { Text(error).foregroundStyle(.red) }
                else { Text("服务器下载完成后会直接加密上传到 Google Drive，手机不用保持连接。") }
            }
            Section("服务器上的任务") {
                if !reachable {
                    Text("连接不上服务器").foregroundStyle(.secondary)
                } else if jobs.isEmpty {
                    Text("没有任务").foregroundStyle(.secondary)
                }
                ForEach(jobs) { t in
                    TransferRow(t: t) { action in
                        Task {
                            try? await model.server?.act(t.id, action)
                            await refresh()
                        }
                    }
                }
            }
        }
        .navigationTitle("离线下载")
        .navigationBarTitleDisplayMode(.inline)
        .task {
            while !Task.isCancelled {
                await refresh()
                try? await Task.sleep(nanoseconds: 2_000_000_000)
            }
        }
    }

    private func refresh() async {
        guard let server = model.server else { return }
        do {
            jobs = try await server.transfers()
            reachable = true
        } catch {
            reachable = false
        }
    }

    private func add() {
        guard let server = model.server else { return }
        let url = link.trimmingCharacters(in: .whitespaces)
        Task {
            do {
                let _: Transfer = try await server.post("api/transfers", ["kind": "url", "url": url, "remote": folder])
                link = ""
                error = nil
                await refresh()
            } catch let e {
                error = e.localizedDescription
            }
        }
    }
}
