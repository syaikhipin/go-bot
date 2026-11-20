export const apiVersion = '1'
export class HostError extends Error {
  constructor(code, message) {
    super(message)
    this.name = 'HostError'
    this.code = code
  }
}
