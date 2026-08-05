export interface LoginRequest {
  email: string
  password: string
}

export interface EmailCodeRequest {
  email: string
}

export interface EmailCodeResponse {
  success: boolean
}

export interface RegisterRequest {
  email: string
  password: string
  code: string
}

export interface AuthTokenResponse {
  strategy: string
  subject: string
  accessToken: string
}
