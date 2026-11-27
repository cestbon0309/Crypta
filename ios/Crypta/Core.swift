import Foundation
import CryptaCore

struct Item: Codable, Identifiable, Hashable {
    let name: String
    let path: String
    let isDir: Bool
    let size: Int64
    let modTime: Date
    var id: String { path }

    enum Kind { case folder, image, video, audio, text, other }

    var kind: Kind {
        if isDir { return .folder }
        let ext = (name as NSString).pathExtension.lowercased()
        if ["jpg", "jpeg", "png", "gif", "heic", "heif", "webp", "bmp", "tiff", "tif"].contains(ext) { return .image }
        if ["mp4", "mkv", "mov", "m4v", "avi", "rmvb", "rm", "wmv", "flv", "webm", "ts", "m2ts", "mpg", "mpeg", "3gp", "vob", "ogv", "asf"].contains(ext) { return .video }
        if ["mp3", "m4a", "flac", "wav", "aac", "ogg", "opus", "wma", "ape"].contains(ext) { return .audio }
        if ["txt", "md", "json", "log", "xml", "csv", "srt", "ass", "ssa", "vtt", "yaml", "yml", "ini", "conf", "nfo", "html", "css", "js", "py", "go", "swift", "sh", "c", "h", "cpp", "java", "toml"].contains(ext) { return .text }
        return .other
    }

    var symbol: String {
        switch kind {
        case .folder: return "folder.fill"
        case .image: return "photo"
        case .video: return "film"
        case .audio: return "music.note"
        case .text: return "doc.text"
        case .other: return "doc"
        }
    }
}

struct Transfer: Codable, Identifiable, Equatable {
    let id: String
    let kind: String
    let state: String
    var phase: String?
    var local: String?
    let remote: String
    let name: String
    let size: Int64
    let done: Int64
    let speed: Int64
    var error: String?

    var isActive: Bool { state == "running" || state == "queued" }
    var fraction: Double { state == "done" ? 1 : (size > 0 ? min(1, Double(done) / Double(size)) : 0) }
}

struct SetupStatus: Codable, Equatable {
    var configured = false
    var demo = false
    var backend = ""
    var server = ""
    var serverToken = ""
}

struct APIError: LocalizedError {
    let message: String
    var errorDescription: String? { message }
}

/// A small client for the Crypta HTTP API. One instance talks to the core
/// running inside the app, another to the paired server.
struct API {
    let base: URL
    let token: String

    private static let decoder: JSONDecoder = {
        let d = JSONDecoder()
        let frac = ISO8601DateFormatter()
        frac.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let plain = ISO8601DateFormatter()
        d.dateDecodingStrategy = .custom { dec in
            let s = try dec.singleValueContainer().decode(String.self)
            return frac.date(from: s) ?? plain.date(from: s) ?? .distantPast
        }
        return d
    }()

    private struct Failure: Decodable { let error: String }
    struct Empty: Decodable {}

    func url(_ path: String, _ query: [String: String] = [:]) -> URL {
        var c = URLComponents(url: base.appendingPathComponent(path), resolvingAgainstBaseURL: false)!
        if !query.isEmpty {
            c.queryItems = query.map { URLQueryItem(name: $0.key, value: $0.value) }
            // "+" must be escaped or it is read as a space.
            c.percentEncodedQuery = c.percentEncodedQuery?.replacingOccurrences(of: "+", with: "%2B")
        }
        return c.url!
    }

    /// URL of a decrypted file for players that cannot send headers.
    func fileURL(_ path: String) -> URL { url("api/file", ["path": path, "token": token]) }

    func send<T: Decodable>(_ method: String, _ path: String, query: [String: String] = [:], body: [String: Any]? = nil, timeout: TimeInterval = 60) async throws -> T {
        var req = URLRequest(url: url(path, query), timeoutInterval: timeout)
        req.httpMethod = method
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        if let body {
            req.httpBody = try JSONSerialization.data(withJSONObject: body)
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let (data, resp) = try await URLSession.shared.data(for: req)
        guard let http = resp as? HTTPURLResponse else { throw APIError(message: "没有响应") }
        guard http.statusCode == 200 else {
            let msg = (try? JSONDecoder().decode(Failure.self, from: data))?.error ?? "HTTP \(http.statusCode)"
            throw APIError(message: msg)
        }
        return try Self.decoder.decode(T.self, from: data)
    }

    func get<T: Decodable>(_ path: String, _ query: [String: String] = [:]) async throws -> T {
        try await send("GET", path, query: query)
    }

    func post<T: Decodable>(_ path: String, _ body: [String: Any] = [:], timeout: TimeInterval = 60) async throws -> T {
        try await send("POST", path, body: body, timeout: timeout)
    }

    func data(_ path: String, range: ClosedRange<Int64>? = nil) async throws -> Data {
        var req = URLRequest(url: url("api/file", ["path": path]))
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        if let range { req.setValue("bytes=\(range.lowerBound)-\(range.upperBound)", forHTTPHeaderField: "Range") }
        let (data, resp) = try await URLSession.shared.data(for: req)
        guard let http = resp as? HTTPURLResponse, (200...299).contains(http.statusCode) else {
            let msg = (try? JSONDecoder().decode(Failure.self, from: data))?.error ?? "读取失败"
            throw APIError(message: msg)
        }
        return data
    }

    // MARK: calls

    struct ItemList: Decodable { let items: [Item] }
    struct TransferList: Decodable { let transfers: [Transfer] }

    func list(_ path: String, fresh: Bool = false) async throws -> [Item] {
        let r: ItemList = try await get("api/list", ["path": path, "fresh": fresh ? "1" : "0"])
        return r.items
    }

    func transfers() async throws -> [Transfer] {
        let r: TransferList = try await get("api/transfers")
        return r.transfers
    }

    func act(_ id: String, _ action: String) async throws {
        let _: Empty = try await post("api/transfers/\(id)/\(action)")
    }
}

enum Paths {
    static let documents = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
    static let state = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("core")
    static let uploads = documents.appendingPathComponent(".uploads")
    static let downloads = documents.appendingPathComponent("Downloads")

    /// Path relative to Documents, which is how the core stores local paths
    /// (the absolute container path changes between installs).
    static func relative(_ url: URL) -> String {
        let root = documents.standardizedFileURL.path
        let p = url.standardizedFileURL.path
        return p.hasPrefix(root + "/") ? String(p.dropFirst(root.count + 1)) : p
    }
}

func formatSize(_ n: Int64) -> String {
    ByteCountFormatter.string(fromByteCount: n, countStyle: .file)
}

func joinPath(_ dir: String, _ name: String) -> String {
    dir.isEmpty ? name : dir + "/" + name
}

func parentPath(_ path: String) -> String {
    guard let i = path.lastIndex(of: "/") else { return "" }
    return String(path[..<i])
}

@MainActor
final class AppModel: ObservableObject {
    @Published var api: API?
    @Published var startError: String?
    @Published var setup = SetupStatus()
    @Published var loaded = false
    @Published var transfers: [Transfer] = []
    /// Bumped whenever an upload finishes, so open folders reload.
    @Published var uploadsFinished = 0
    @Published var pendingPair: (server: String, code: String)?

    private var doneIDs = Set<String>()
    private var poller: Task<Void, Never>?

    var server: API? {
        guard !setup.server.isEmpty, let u = URL(string: setup.server) else { return nil }
        return API(base: u, token: setup.serverToken)
    }

    func start() {
        guard api == nil else { return }
        do {
            try FileManager.default.createDirectory(at: Paths.state, withIntermediateDirectories: true)
            try FileManager.default.createDirectory(at: Paths.downloads, withIntermediateDirectories: true)
            var err: NSError?
            let out = MobileStart(Paths.state.path, Paths.documents.path, &err)
            if let err { throw err }
            let info = try JSONDecoder().decode([String: String].self, from: Data(out.utf8))
            guard let u = info["url"].flatMap(URL.init(string:)), let t = info["token"] else { throw APIError(message: "核心启动失败") }
            api = API(base: u, token: t)
        } catch {
            startError = error.localizedDescription
            return
        }
        Task { await refreshSetup() }
        poller = Task { [weak self] in
            while !Task.isCancelled {
                await self?.pollTransfers()
                try? await Task.sleep(nanoseconds: 1_000_000_000)
            }
        }
    }

    func refreshSetup() async {
        guard let api else { return }
        if let s: SetupStatus = try? await api.get("api/setup") { setup = s }
        loaded = true
    }

    func pollTransfers() async {
        guard let api, setup.configured, let list = try? await api.transfers() else { return }
        if list != transfers { transfers = list }
        let done = Set(list.filter { $0.state == "done" && $0.kind != "download" }.map(\.id))
        if !done.subtracting(doneIDs).isEmpty { uploadsFinished += 1 }
        doneIDs = done
    }

    /// Runs a setup call (import, pair, demo, reset) and adopts the result.
    func configure(_ path: String, _ body: [String: Any] = [:]) async throws {
        guard let api else { return }
        setup = try await api.post(path, body, timeout: 90)
        // Previews belong to the storage that was just replaced.
        await ThumbnailStore.shared.clear()
        transfers = []
        await pollTransfers()
    }

    func handle(url: URL) {
        guard url.scheme == "crypta", url.host == "pair",
              let items = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems,
              let server = items.first(where: { $0.name == "server" })?.value,
              let code = items.first(where: { $0.name == "code" })?.value else { return }
        pendingPair = (server, code)
    }

    /// Copies a picked file into the app and queues it for upload.
    func upload(file: URL, to folder: String, move: Bool = false) async throws {
        guard let api else { return }
        let dir = Paths.uploads.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let dest = dir.appendingPathComponent(file.lastPathComponent)
        let scoped = file.startAccessingSecurityScopedResource()
        defer { if scoped { file.stopAccessingSecurityScopedResource() } }
        if move { try FileManager.default.moveItem(at: file, to: dest) } else { try FileManager.default.copyItem(at: file, to: dest) }
        let _: Transfer = try await api.post("api/transfers", ["kind": "upload", "local": Paths.relative(dest), "remote": folder, "deleteSource": true])
        await pollTransfers()
    }

    func download(_ item: Item) async throws {
        guard let api else { return }
        let _: Transfer = try await api.post("api/transfers", ["kind": "download", "remote": item.path, "local": "Downloads/" + item.name])
        await pollTransfers()
    }
}
