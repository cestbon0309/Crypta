import CryptoKit
import ImageIO
import SwiftUI

enum Prefs {
    static let viewMode = "browser.viewMode"
    static let sortKey = "browser.sortKey"
    static let sortAscending = "browser.sortAscending"
    static let showThumbnails = "browser.showThumbnails"
}

enum ViewMode: String, CaseIterable, Identifiable {
    case icons, list, details
    var id: String { rawValue }

    var title: String {
        switch self {
        case .icons: return "图标"
        case .list: return "列表"
        case .details: return "详细信息"
        }
    }

    var symbol: String {
        switch self {
        case .icons: return "square.grid.2x2"
        case .list: return "list.bullet"
        case .details: return "list.bullet.below.rectangle"
        }
    }
}

enum SortKey: String, CaseIterable, Identifiable {
    case name, date, size
    var id: String { rawValue }

    var title: String {
        switch self {
        case .name: return "名称"
        case .date: return "修改日期"
        case .size: return "大小"
        }
    }

    /// Folders always come first; ties fall back to the name.
    func sorted(_ items: [Item], ascending: Bool) -> [Item] {
        items.sorted { a, b in
            if a.isDir != b.isDir { return a.isDir }
            let byName = a.name.localizedStandardCompare(b.name)
            let order: ComparisonResult
            switch self {
            case .name: order = byName
            case .date: order = a.modTime == b.modTime ? byName : (a.modTime < b.modTime ? .orderedAscending : .orderedDescending)
            case .size: order = a.size == b.size ? byName : (a.size < b.size ? .orderedAscending : .orderedDescending)
            }
            return ascending ? order == .orderedAscending : order == .orderedDescending
        }
    }
}

extension Item {
    var kindName: String {
        switch kind {
        case .folder: return "文件夹"
        case .image: return "图片"
        case .video: return "视频"
        case .audio: return "音频"
        case .text: return "文本"
        case .other:
            let ext = (name as NSString).pathExtension.uppercased()
            return ext.isEmpty ? "文件" : "\(ext) 文件"
        }
    }

    var dateText: String { modTime.formatted(date: .abbreviated, time: .shortened) }
}

// MARK: - Thumbnails

/// Small previews of images and videos, kept on disk. The server stores one
/// next to what it uploads; an image without one is fetched and made small
/// on the phone.
actor ThumbnailStore {
    static let shared = ThumbnailStore()
    /// Larger images are not fetched just to make a preview.
    static let maxBytes: Int64 = 30 << 20
    private static let pixels = 320

    private let memory = NSCache<NSString, UIImage>()
    private let dir = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0].appendingPathComponent("thumbnails")
    private var loading: [String: Task<UIImage?, Never>] = [:]
    private var running = 0
    private var waiting: [CheckedContinuation<Void, Never>] = []
    /// Bumped by `clear`, so a fetch that started earlier saves nothing.
    private var generation = 0

    init() {
        memory.countLimit = 400
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    }

    private static func key(_ item: Item) -> String {
        let text = "\(item.path)|\(item.size)|\(item.modTime.timeIntervalSince1970)"
        return SHA256.hash(data: Data(text.utf8)).map { String(format: "%02x", $0) }.joined()
    }

    func image(for item: Item, api: API) async -> UIImage? {
        guard item.kind == .image || item.kind == .video else { return nil }
        let key = Self.key(item)
        if let hit = memory.object(forKey: key as NSString) { return hit }
        if let task = loading[key] { return await task.value }
        let file = dir.appendingPathComponent(key + ".jpg")
        let started = generation
        let task = Task<UIImage?, Never> {
            if let saved = UIImage(contentsOfFile: file.path) { return saved }
            await self.acquire()
            defer { self.release() }
            guard !Task.isCancelled else { return nil }
            if let data = try? await api.data(item.path, from: "api/thumb"), let image = UIImage(data: data) {
                guard self.generation == started else { return nil }
                try? data.write(to: file, options: .atomic)
                return image
            }
            guard !Task.isCancelled, item.kind == .image, item.size <= Self.maxBytes,
                  let data = try? await api.data(item.path),
                  let image = Self.downsample(data), self.generation == started else { return nil }
            try? image.jpegData(compressionQuality: 0.8)?.write(to: file, options: .atomic)
            return image
        }
        loading[key] = task
        let image = await task.value
        guard generation == started else { return nil }
        loading[key] = nil
        if let image { memory.setObject(image, forKey: key as NSString) }
        return image
    }

    func clear() {
        generation += 1
        loading.values.forEach { $0.cancel() }
        loading = [:]
        memory.removeAllObjects()
        try? FileManager.default.removeItem(at: dir)
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    }

    private func acquire() async {
        if running < 3 { running += 1; return }
        await withCheckedContinuation { waiting.append($0) }
    }

    private func release() {
        if waiting.isEmpty { running -= 1 } else { waiting.removeFirst().resume() }
    }

    private static func downsample(_ data: Data) -> UIImage? {
        guard let source = CGImageSourceCreateWithData(data as CFData, [kCGImageSourceShouldCache: false] as CFDictionary) else { return nil }
        let options: [CFString: Any] = [
            kCGImageSourceCreateThumbnailFromImageAlways: true,
            kCGImageSourceCreateThumbnailWithTransform: true,
            kCGImageSourceShouldCacheImmediately: true,
            kCGImageSourceThumbnailMaxPixelSize: pixels,
        ]
        guard let cg = CGImageSourceCreateThumbnailAtIndex(source, 0, options as CFDictionary) else { return nil }
        return UIImage(cgImage: cg)
    }
}

/// The square picture in front of an item: its thumbnail, or a symbol on a
/// gray tile.
struct ItemIcon: View {
    @EnvironmentObject var model: AppModel
    @AppStorage(Prefs.showThumbnails) private var showThumbnails = true
    let item: Item
    let size: CGFloat

    @State private var thumbnail: UIImage?

    private var wantsThumbnail: Bool {
        showThumbnails && (item.kind == .image || item.kind == .video)
    }

    var body: some View {
        let shape = RoundedRectangle(cornerRadius: size * 0.22, style: .continuous)
        ZStack {
            if let thumbnail, wantsThumbnail {
                Image(uiImage: thumbnail).resizable().scaledToFill()
                if item.kind == .video {
                    Image(systemName: "play.fill")
                        .font(.system(size: size * 0.24))
                        .foregroundStyle(.white)
                        .shadow(color: .black.opacity(0.6), radius: 2)
                }
            } else {
                shape.fill(Color(.tertiarySystemFill))
                Image(systemName: item.symbol)
                    .font(.system(size: size * 0.46, weight: .regular))
                    .foregroundStyle(item.isDir ? Color.primary.opacity(0.72) : Color.secondary)
            }
        }
        .frame(width: size, height: size)
        .clipShape(shape)
        .overlay(shape.strokeBorder(Color.primary.opacity(0.06)))
        .task(id: wantsThumbnail ? "\(item.id)|\(item.size)|\(item.modTime.timeIntervalSince1970)" : "") {
            guard wantsThumbnail, let api = model.api else { thumbnail = nil; return }
            thumbnail = await ThumbnailStore.shared.image(for: item, api: api)
        }
    }
}

struct SelectionMark: View {
    let selected: Bool

    var body: some View {
        Image(systemName: selected ? "checkmark.circle.fill" : "circle")
            .font(.title3)
            .symbolRenderingMode(.palette)
            .foregroundStyle(selected ? Color.white : Color(.tertiaryLabel), selected ? Color.accentColor : Color.clear)
    }
}

// MARK: - Rows and cells

struct ItemRow: View {
    let item: Item
    var detailed = true
    /// nil when the list is not in selection mode.
    var selected: Bool?

    var body: some View {
        HStack(spacing: 12) {
            if let selected { SelectionMark(selected: selected) }
            ItemIcon(item: item, size: detailed ? 44 : 30)
            if detailed {
                VStack(alignment: .leading, spacing: 3) {
                    Text(item.name).lineLimit(2)
                    Text(item.isDir ? item.dateText : "\(item.dateText) · \(formatSize(item.size))")
                        .font(.caption).foregroundStyle(.secondary)
                }
            } else {
                Text(item.name).lineLimit(1)
            }
            Spacer(minLength: 0)
        }
        .contentShape(Rectangle())
        .padding(.vertical, detailed ? 3 : 0)
    }
}

struct ItemCell: View {
    let item: Item
    var selected: Bool?

    var body: some View {
        VStack(spacing: 6) {
            ItemIcon(item: item, size: 72)
                .overlay(alignment: .bottomTrailing) {
                    if let selected {
                        SelectionMark(selected: selected)
                            .background(Circle().fill(Color(.systemBackground)).padding(2))
                            .offset(x: 6, y: 6)
                    }
                }
            Text(item.name)
                .font(.caption)
                .lineLimit(2)
                .multilineTextAlignment(.center)
                .frame(maxWidth: .infinity, minHeight: 32, alignment: .top)
        }
        .padding(6)
        .contentShape(RoundedRectangle(cornerRadius: 12, style: .continuous))
    }
}

// MARK: - Details

struct ItemInfoView: View {
    @Environment(\.dismiss) private var dismiss
    let item: Item

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    VStack(spacing: 10) {
                        ItemIcon(item: item, size: 88)
                        Text(item.name).font(.headline).multilineTextAlignment(.center).textSelection(.enabled)
                    }
                    .frame(maxWidth: .infinity)
                    .listRowBackground(Color.clear)
                }
                Section {
                    LabeledContent("类型", value: item.kindName)
                    if !item.isDir { LabeledContent("大小", value: "\(formatSize(item.size))（\(item.size.formatted()) 字节）") }
                    LabeledContent("修改日期", value: item.modTime.formatted(date: .long, time: .shortened))
                    LabeledContent("位置", value: "/" + parentPath(item.path))
                }
            }
            .navigationTitle("详细信息")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button("完成") { dismiss() } } }
        }
        .presentationDetents([.medium, .large])
    }
}

/// The folders leading to the current one; tapping one goes back to it.
struct PathBar: View {
    let path: String
    let go: (Int) -> Void

    var body: some View {
        let parts = path.split(separator: "/").map(String.init)
        // The whole path if it fits, otherwise its last folders.
        ViewThatFits(in: .horizontal) {
            row(parts, from: 0)
            if parts.count > 2 { row(parts, from: parts.count - 2) }
            if parts.count > 1 { row(parts, from: parts.count - 1) }
        }
        .font(.footnote)
        .foregroundStyle(.secondary)
        .buttonStyle(.plain)
        .padding(.horizontal, 16).padding(.vertical, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.bar)
        .overlay(alignment: .bottom) { Divider() }
    }

    private func row(_ parts: [String], from start: Int) -> some View {
        HStack(spacing: 6) {
            Button { go(0) } label: { Image(systemName: "house") }
                .accessibilityLabel("Crypta")
            if start > 0 {
                Image(systemName: "chevron.right").font(.caption2).foregroundStyle(.tertiary)
                Menu {
                    ForEach(Array(parts.prefix(start).enumerated()), id: \.offset) { i, name in
                        Button(name) { go(i + 1) }
                    }
                } label: {
                    Text("…").padding(.horizontal, 4)
                }
                .accessibilityLabel("上级文件夹")
            }
            ForEach(Array(parts.enumerated()).dropFirst(start), id: \.offset) { i, name in
                Image(systemName: "chevron.right").font(.caption2).foregroundStyle(.tertiary)
                if i == parts.count - 1 {
                    Text(name).fontWeight(.medium).foregroundStyle(.primary)
                } else {
                    Button(name) { go(i + 1) }
                }
            }
        }
        .lineLimit(1)
    }
}
