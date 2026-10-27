export namespace main {
	
	export class ConnectResult {
	    sid?: string;
	    error?: string;
	    kind?: string;
	    addr?: string;
	    fingerprint?: string;
	    keyType?: string;
	
	    static createFrom(source: any = {}) {
	        return new ConnectResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.sid = source["sid"];
	        this.error = source["error"];
	        this.kind = source["kind"];
	        this.addr = source["addr"];
	        this.fingerprint = source["fingerprint"];
	        this.keyType = source["keyType"];
	    }
	}
	export class FileEntry {
	    name: string;
	    path: string;
	    isDir: boolean;
	    size: number;
	    owner: string;
	    group: string;
	    mode: string;
	    mtime: string;
	
	    static createFrom(source: any = {}) {
	        return new FileEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.path = source["path"];
	        this.isDir = source["isDir"];
	        this.size = source["size"];
	        this.owner = source["owner"];
	        this.group = source["group"];
	        this.mode = source["mode"];
	        this.mtime = source["mtime"];
	    }
	}
	export class ProfileInfo {
	    id: string;
	    name: string;
	    host: string;
	    port: number;
	    auth: string;
	    keyPath: string;
	
	    static createFrom(source: any = {}) {
	        return new ProfileInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.host = source["host"];
	        this.port = source["port"];
	        this.auth = source["auth"];
	        this.keyPath = source["keyPath"];
	    }
	}
	export class QueueItem {
	    id: number;
	    name: string;
	    direction: string;
	    status: string;
	    progress: number;
	    speed: string;
	    eta: string;
	    transferred: string;
	    total: string;
	    error: string;
	
	    static createFrom(source: any = {}) {
	        return new QueueItem(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.direction = source["direction"];
	        this.status = source["status"];
	        this.progress = source["progress"];
	        this.speed = source["speed"];
	        this.eta = source["eta"];
	        this.transferred = source["transferred"];
	        this.total = source["total"];
	        this.error = source["error"];
	    }
	}
	export class SessionInfo {
	    id: string;
	    name: string;
	    host: string;
	    user: string;
	    state: string;
	    filesPath: string;
	
	    static createFrom(source: any = {}) {
	        return new SessionInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.host = source["host"];
	        this.user = source["user"];
	        this.state = source["state"];
	        this.filesPath = source["filesPath"];
	    }
	}
	export class TunnelInfo {
	    id: string;
	    sessionId: string;
	    name: string;
	    type: string;
	    kind: string;
	    bind: string;
	    localPort: number;
	    remoteHost: string;
	    remotePort: number;
	    remote: string;
	    running: boolean;
	
	    static createFrom(source: any = {}) {
	        return new TunnelInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.sessionId = source["sessionId"];
	        this.name = source["name"];
	        this.type = source["type"];
	        this.kind = source["kind"];
	        this.bind = source["bind"];
	        this.localPort = source["localPort"];
	        this.remoteHost = source["remoteHost"];
	        this.remotePort = source["remotePort"];
	        this.remote = source["remote"];
	        this.running = source["running"];
	    }
	}

}

