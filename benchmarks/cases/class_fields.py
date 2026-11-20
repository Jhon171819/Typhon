class Point:
    x: int
    y: int

    def __init__(self, x: int, y: int) -> None:
        self.x = x
        self.y = y


def make_total() -> int:
    point: Point = Point(20, 22)
    return point.x + point.y


print(make_total())
